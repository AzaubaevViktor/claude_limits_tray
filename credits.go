package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// credits.go models the pay-as-you-go credit budget both providers expose next
// to their rate limits: Claude's "extra usage" / usage-credit spend and Codex's
// credit balance. Credits are only consumed once a plan window is exhausted,
// so the useful question is not "how many credits are left" but "do the
// credits last until the next reset at the pace they are burning now". That
// projection drives the thin outer dial ring, the credits menu rows, and the
// credits burn chart.

// creditBudget is a provider's credit balance in display units (dollars for
// Claude, "cr" for Codex). Known is false when the provider reports no usable
// credit budget at all (Claude extra usage disabled, Codex has_credits false).
type creditBudget struct {
	Known      bool
	Unlimited  bool
	Left       float64 // remaining credits
	LeftKnown  bool
	Used       float64 // spent in the current budget period
	UsedKnown  bool
	Limit      float64 // the period's cap
	LimitKnown bool
	Unit       string // "$"/"€"/"£" prefix currencies; anything else is a suffix
}

// usedPct is the share of the period cap already spent, when both are known.
func (b creditBudget) usedPct() (float64, bool) {
	if !b.LimitKnown || b.Limit <= 0 || !b.UsedKnown {
		return 0, false
	}
	return clamp(b.Used/b.Limit*100, 0, 100), true
}

// creditProjection answers whether the credits last until the horizon reset at
// one explicit burn rate. Cover is the share of the time to reset the balance
// covers (1 = lasts); DaySlice is how much of that same ring the next 24 hours
// occupy, capped at Cover, so the lighter head of the arc reads "this is what
// one more day costs".
type creditProjection struct {
	Projected  bool    // Cover/DaySlice are meaningful (balance and reset known)
	Empty      bool    // balance exhausted
	Cover      float64 // 0..1
	DaySlice   float64 // 0..1
	Runway     float64 // seconds the balance lasts; +Inf while nothing is burning
	ResetSecs  float64 // seconds to the horizon reset (when ResetKnown)
	ResetKnown bool
	DailySpend float64 // credits per day at the rate
}

// projectCredits projects a credit budget against the time to the horizon reset
// at ratePerSec (display units per second); rateKnown is false while no rate
// has been measured yet.
func projectCredits(b creditBudget, ratePerSec float64, rateKnown bool, resetSecs float64, resetKnown bool) creditProjection {
	p := creditProjection{
		Runway:     math.Inf(1),
		ResetSecs:  resetSecs,
		ResetKnown: resetKnown && resetSecs > 0,
		DailySpend: math.Max(0, ratePerSec) * 86400,
	}
	if !b.Known {
		return p
	}
	if b.Unlimited {
		p.Projected, p.Cover = true, 1
		return p
	}
	if !b.LeftKnown {
		return p
	}
	if b.Left <= 0 {
		p.Projected, p.Empty, p.Runway = true, true, 0
		return p
	}
	if ratePerSec > 1e-12 {
		p.Runway = b.Left / ratePerSec
	}
	// An unmeasured rate (one sample, or stale polls) is not "idle": leave the
	// ring as a plain track rather than claiming the balance lasts.
	if !p.ResetKnown || !rateKnown {
		return p
	}
	p.Projected = true
	if math.IsInf(p.Runway, 1) {
		p.Cover = 1
		return p
	}
	p.Cover = clamp(p.Runway/resetSecs, 0, 1)
	p.DaySlice = math.Min(86400/resetSecs, p.Cover)
	return p
}

// key folds the ring into an icon cache key at the drawn resolution.
func (r creditRing) key() string {
	return fmt.Sprintf("c%t%t%t-%d-%d", r.Show, r.Projected, r.Empty,
		iround(r.Cover*48), iround(r.DaySlice*48))
}

// ring converts a projection into the dial's thin outer ring. show is whether
// the provider has a credit budget at all; without one the ring is omitted so
// the icon looks exactly as before.
func (p creditProjection) ring(show bool) creditRing {
	return creditRing{Show: show, Projected: p.Projected, Empty: p.Empty,
		Cover: p.Cover, DaySlice: p.DaySlice}
}

// --- parsing -------------------------------------------------------------

// claudeCredits reads Claude's credit budget. The newer "spend" object (usage
// credits with a balance) wins when enabled; otherwise the older "extra_usage"
// monthly limit is used. Both are parsed leniently from raw JSON so a shape
// change can never break the whole usage response.
func claudeCredits(u *Usage) creditBudget {
	if u == nil {
		return creditBudget{}
	}
	if b, ok := claudeSpendCredits(u.Spend); ok {
		return b
	}
	if b, ok := claudeExtraUsageCredits(u.ExtraUsage); ok {
		return b
	}
	return creditBudget{}
}

func claudeSpendCredits(raw json.RawMessage) (creditBudget, bool) {
	var s struct {
		Enabled bool            `json:"enabled"`
		Used    json.RawMessage `json:"used"`
		Limit   json.RawMessage `json:"limit"`
		Balance json.RawMessage `json:"balance"`
		Cap     json.RawMessage `json:"cap"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil || !s.Enabled {
		return creditBudget{}, false
	}
	b := creditBudget{Known: true}
	currency := ""
	if v, cur, ok := lenientAmount(s.Used); ok {
		b.Used, b.UsedKnown = v, true
		currency = orElse(currency, cur)
	}
	for _, lim := range []json.RawMessage{s.Limit, s.Cap} {
		if v, cur, ok := lenientAmount(lim); ok && v > 0 {
			b.Limit, b.LimitKnown = v, true
			currency = orElse(currency, cur)
			break
		}
	}
	if v, cur, ok := lenientAmount(s.Balance); ok {
		b.Left, b.LeftKnown = math.Max(0, v), true
		currency = orElse(currency, cur)
	} else if b.LimitKnown && b.UsedKnown {
		b.Left, b.LeftKnown = math.Max(0, b.Limit-b.Used), true
	}
	b.Unit = currencyUnit(currency)
	return b, true
}

// claudeExtraUsageCredits reads the monthly extra-usage limit. Amounts are
// taken to be in minor units scaled by decimal_places (like spend.amount_minor
// with its exponent); when decimal_places is absent they are used as-is.
func claudeExtraUsageCredits(raw json.RawMessage) (creditBudget, bool) {
	var e struct {
		IsEnabled     bool            `json:"is_enabled"`
		MonthlyLimit  json.RawMessage `json:"monthly_limit"`
		UsedCredits   json.RawMessage `json:"used_credits"`
		Currency      string          `json:"currency"`
		DecimalPlaces json.RawMessage `json:"decimal_places"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &e) != nil || !e.IsEnabled {
		return creditBudget{}, false
	}
	scale := 1.0
	if dp, _, ok := lenientAmount(e.DecimalPlaces); ok && dp > 0 {
		scale = math.Pow(10, -dp)
	}
	b := creditBudget{Known: true, Unit: currencyUnit(e.Currency)}
	if v, _, ok := lenientAmount(e.UsedCredits); ok {
		b.Used, b.UsedKnown = v*scale, true
	}
	if v, _, ok := lenientAmount(e.MonthlyLimit); ok && v > 0 {
		b.Limit, b.LimitKnown = v*scale, true
	}
	if b.LimitKnown && b.UsedKnown {
		b.Left, b.LeftKnown = math.Max(0, b.Limit-b.Used), true
	}
	return b, true
}

// codexCredits reads the Codex credit balance. A user who never bought credits
// (has_credits false, no balance) has no budget to project, not an empty one.
func codexCredits(u *CodexUsage) creditBudget {
	if u == nil || len(u.Limits) == 0 || u.Limits[0].Credits == nil {
		return creditBudget{}
	}
	c := u.Limits[0].Credits
	if c.Unlimited {
		return creditBudget{Known: true, Unlimited: true, Unit: "cr"}
	}
	b := creditBudget{Unit: "cr"}
	if c.Balance != nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(*c.Balance), 64); err == nil {
			b.Left, b.LeftKnown = math.Max(0, v), true
		}
	}
	b.Known = c.HasCredits || (b.LeftKnown && b.Left > 0)
	if !b.Known {
		return creditBudget{}
	}
	return b
}

// lenientAmount reads a money-ish JSON value — a number, a numeric string, or
// an object carrying amount_minor (+ exponent) or amount — as a value in major
// units, plus the currency when the object names one.
func lenientAmount(raw json.RawMessage) (float64, string, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, "", false
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f, "", true
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		if f, err := strconv.ParseFloat(strings.TrimSpace(str), 64); err == nil {
			return f, "", true
		}
		return 0, "", false
	}
	var obj struct {
		AmountMinor json.RawMessage `json:"amount_minor"`
		Amount      json.RawMessage `json:"amount"`
		Exponent    *int            `json:"exponent"`
		Currency    string          `json:"currency"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return 0, "", false
	}
	if v, _, ok := lenientAmount(obj.AmountMinor); ok {
		exp := 2
		if obj.Exponent != nil {
			exp = *obj.Exponent
		}
		return v / math.Pow(10, float64(exp)), obj.Currency, true
	}
	if v, _, ok := lenientAmount(obj.Amount); ok {
		return v, obj.Currency, true
	}
	return 0, "", false
}

func currencyUnit(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "", "USD":
		return "$"
	case "EUR":
		return "€"
	case "GBP":
		return "£"
	default:
		return strings.ToUpper(strings.TrimSpace(code))
	}
}

// --- formatting ----------------------------------------------------------

// fmtAmount renders a credit amount in its unit: "$12.30", "$1,234",
// "60,874 cr", "3.25 cr".
func fmtAmount(v float64, unit string) string {
	av := math.Abs(v)
	prefix := unit == "$" || unit == "€" || unit == "£"
	var num string
	switch {
	case av >= 1000:
		num = groupThousands(fmt.Sprintf("%.0f", math.Round(v)))
	case prefix || av < 10:
		num = fmt.Sprintf("%.2f", v)
	default:
		num = fmt.Sprintf("%.1f", v)
	}
	switch {
	case prefix:
		return unit + num
	case unit == "":
		return num
	default:
		return num + " " + unit
	}
}

func groupThousands(digits string) string {
	neg := strings.HasPrefix(digits, "-")
	digits = strings.TrimPrefix(digits, "-")
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// creditsLine is the headline credits menu row.
func creditsLine(b creditBudget) string {
	switch {
	case !b.Known:
		return "Credits: —"
	case b.Unlimited:
		return "Credits: unlimited"
	}
	parts := []string{}
	if b.LeftKnown {
		parts = append(parts, fmtAmount(b.Left, b.Unit)+" left")
	}
	if b.LimitKnown {
		parts = append(parts, "of "+fmtAmount(b.Limit, b.Unit))
	}
	if pct, ok := b.usedPct(); ok {
		parts = append(parts, fmt.Sprintf("(%.0f%% used)", pct))
	} else if b.UsedKnown {
		parts = append(parts, "("+fmtAmount(b.Used, b.Unit)+" used)")
	}
	if len(parts) == 0 {
		return "Credits: available"
	}
	return "Credits: " + strings.Join(parts, " ")
}

// creditsRunwayLine is the indented detail row under the credits headline:
// how long the balance lasts at the recent rate against the time to reset, and
// what one more day costs at that rate.
func creditsRunwayLine(b creditBudget, p creditProjection, rateKnown bool, rateSpan float64) string {
	if !b.Known {
		return ""
	}
	reset := fmtRemaining(p.ResetSecs, p.ResetKnown)
	daily := ""
	if rateKnown && p.DailySpend > 0 {
		daily = " · ≈ " + fmtAmount(p.DailySpend, b.Unit) + "/day (last " + spanLabel(rateSpan) + ")"
	}
	switch {
	case b.Unlimited:
		return "      unlimited" + daily
	case p.Empty:
		return "      exhausted · " + reset + " to reset"
	case !rateKnown:
		return "      rate — · " + reset + " to reset"
	case math.IsInf(p.Runway, 1):
		return "      idle · lasts to reset (" + reset + ")"
	case !p.ResetKnown:
		return "      lasts " + fmtRemaining(p.Runway, true) + " · reset —" + daily
	case p.Cover >= 1:
		return "      lasts to reset (" + reset + ") · runway " + fmtRemaining(p.Runway, true) + daily
	default:
		return fmt.Sprintf("      lasts %s of %s to reset (%.0f%%)%s",
			fmtRemaining(p.Runway, true), reset, p.Cover*100, daily)
	}
}
