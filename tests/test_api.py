import json
from unittest import mock

import pytest

import api
from api import ApiError, UsageClient


def _proc(stdout, code=0, stderr=""):
    return mock.Mock(returncode=code, stdout=stdout, stderr=stderr)


def test_success():
    c = UsageClient()
    out = json.dumps({"five_hour": {"utilization": 21}}) + c.META + "200|"
    with mock.patch.object(UsageClient, "_token", return_value="t"), \
         mock.patch("api.subprocess.run", return_value=_proc(out)):
        data = c.usage()
    assert data["five_hour"]["utilization"] == 21


def test_429_parses_retry_after_and_reset():
    c = UsageClient()
    out = '{"error":1}' + c.META + "429|0|2026-06-25T21:00:00+00:00"
    with mock.patch.object(UsageClient, "_token", return_value="t"), \
         mock.patch("api.subprocess.run", return_value=_proc(out)):
        with pytest.raises(ApiError) as ei:
            c.usage()
    assert ei.value.status == 429
    assert ei.value.retry_after == 0
    assert ei.value.reset_at == "2026-06-25T21:00:00+00:00"


def test_transport_error():
    c = UsageClient()
    with mock.patch.object(UsageClient, "_token", return_value="t"), \
         mock.patch("api.subprocess.run", return_value=_proc("", code=7)):
        with pytest.raises(ApiError) as ei:
            c.usage()
    assert ei.value.status is None
    assert "Offline" in str(ei.value)


def test_malformed_response():
    c = UsageClient()
    out = "body-without-meta-marker"
    with mock.patch.object(UsageClient, "_token", return_value="t"), \
         mock.patch("api.subprocess.run", return_value=_proc(out)):
        with pytest.raises(ApiError) as ei:
            c.usage()
    assert "Malformed" in str(ei.value)


def test_no_credentials():
    c = UsageClient()
    with mock.patch("api.subprocess.check_output", side_effect=OSError):
        with pytest.raises(ApiError) as ei:
            c.usage()
    assert "No credentials" in str(ei.value)
