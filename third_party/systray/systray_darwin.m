#import <Cocoa/Cocoa.h>
#include "systray.h"

#if __MAC_OS_X_VERSION_MIN_REQUIRED < 101400

    #ifndef NSControlStateValueOff
      #define NSControlStateValueOff NSOffState
    #endif

    #ifndef NSControlStateValueOn
      #define NSControlStateValueOn NSOnState
    #endif

#endif

@interface MenuItem : NSObject
{
  @public
    NSNumber* menuId;
    NSNumber* parentMenuId;
    NSString* title;
    NSString* tooltip;
    short disabled;
    short checked;
}
-(id) initWithId: (int)theMenuId
withParentMenuId: (int)theParentMenuId
       withTitle: (const char*)theTitle
     withTooltip: (const char*)theTooltip
    withDisabled: (short)theDisabled
     withChecked: (short)theChecked;
     @end
     @implementation MenuItem
     -(id) initWithId: (int)theMenuId
     withParentMenuId: (int)theParentMenuId
            withTitle: (const char*)theTitle
          withTooltip: (const char*)theTooltip
         withDisabled: (short)theDisabled
          withChecked: (short)theChecked
{
  menuId = [NSNumber numberWithInt:theMenuId];
  parentMenuId = [NSNumber numberWithInt:theParentMenuId];
  title = [[NSString alloc] initWithCString:theTitle
                                   encoding:NSUTF8StringEncoding];
  tooltip = [[NSString alloc] initWithCString:theTooltip
                                     encoding:NSUTF8StringEncoding];
  disabled = theDisabled;
  checked = theChecked;
  return self;
}
@end

@interface AppDelegate: NSObject <NSApplicationDelegate>
  - (void) add_or_update_menu_item:(MenuItem*) item;
  - (IBAction)menuHandler:(id)sender;
  @property (assign) IBOutlet NSWindow *window;
  @end

  @implementation AppDelegate
{
  NSStatusItem *statusItem;
  NSMenu *menu;
  NSCondition* cond;
  // Borderless click-through window centred on the status item, used to draw
  // animation that has to leave the icon's own rectangle (see showSparkOverlay).
  NSWindow *sparkWindow;
  NSImageView *sparkView;
}

@synthesize window = _window;

- (void)applicationDidFinishLaunching:(NSNotification *)aNotification
{
  self->statusItem = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
  self->menu = [[NSMenu alloc] init];
  [self->menu setAutoenablesItems: FALSE];
  [self->statusItem setMenu:self->menu];
  systray_ready();
}

- (void)applicationWillTerminate:(NSNotification *)aNotification
{
  systray_on_exit();
}

- (void)setIcon:(NSImage *)image {
  statusItem.button.image = image;
  [self updateTitleButtonStyle];
}

- (void)setTitle:(NSString *)title {
  statusItem.button.title = title;
  [self updateTitleButtonStyle];
}

-(void)updateTitleButtonStyle {
  if (statusItem.button.image != nil) {
    if ([statusItem.button.title length] == 0) {
      statusItem.button.imagePosition = NSImageOnly;
    } else {
      statusItem.button.imagePosition = NSImageLeft;
    }
  } else {
    statusItem.button.imagePosition = NSNoImage;
  }
}


// showSpark: args are @[image, width, height] in points. The window is centred
// on the status item and sits just above the menu bar, so whatever the image
// draws around the icon spills over the bar and the screen below it. It never
// takes focus or swallows clicks.
- (void)showSpark:(NSArray *)args {
  NSImage *image = [args objectAtIndex:0];
  CGFloat w = [[args objectAtIndex:1] doubleValue];
  CGFloat h = [[args objectAtIndex:2] doubleValue];
  NSWindow *host = statusItem.button.window;
  if (host == nil || !host.isVisible || w <= 0 || h <= 0) {
    [self hideSpark:nil]; // item hidden (notch overflow, Bartender, ...)
    return;
  }
  NSRect f = host.frame;
  NSRect r = NSMakeRect(NSMidX(f) - w / 2, NSMidY(f) - h / 2, w, h);
  if (self->sparkWindow == nil) {
    self->sparkWindow = [[NSWindow alloc] initWithContentRect:r
                                                    styleMask:NSWindowStyleMaskBorderless
                                                      backing:NSBackingStoreBuffered
                                                        defer:NO];
    [self->sparkWindow setOpaque:NO];
    [self->sparkWindow setBackgroundColor:[NSColor clearColor]];
    [self->sparkWindow setIgnoresMouseEvents:YES];
    [self->sparkWindow setLevel:NSStatusWindowLevel + 1];
    [self->sparkWindow setHidesOnDeactivate:NO];
    [self->sparkWindow setCollectionBehavior:
      NSWindowCollectionBehaviorCanJoinAllSpaces |
      NSWindowCollectionBehaviorStationary |
      NSWindowCollectionBehaviorFullScreenAuxiliary |
      NSWindowCollectionBehaviorIgnoresCycle];
    self->sparkView = [[NSImageView alloc] initWithFrame:NSMakeRect(0, 0, w, h)];
    [self->sparkView setImageScaling:NSImageScaleAxesIndependently];
    [self->sparkWindow setContentView:self->sparkView];
  }
  [self->sparkWindow setFrame:r display:NO];
  [self->sparkView setFrame:NSMakeRect(0, 0, w, h)];
  [image setSize:NSMakeSize(w, h)];
  self->sparkView.image = image;
  if (!self->sparkWindow.isVisible) {
    [self->sparkWindow orderFrontRegardless];
  }
}

- (void)hideSpark:(id)ignored {
  if (self->sparkWindow != nil && self->sparkWindow.isVisible) {
    [self->sparkWindow orderOut:nil];
  }
}

- (void)setTooltip:(NSString *)tooltip {
  statusItem.button.toolTip = tooltip;
}

- (IBAction)menuHandler:(id)sender
{
  NSNumber* menuId = [sender representedObject];
  systray_menu_item_selected(menuId.intValue);
}

- (void)add_or_update_menu_item:(MenuItem *)item {
  NSMenu *theMenu = self->menu;
  NSMenuItem *parentItem;
  if ([item->parentMenuId integerValue] > 0) {
    parentItem = find_menu_item(menu, item->parentMenuId);
    if (parentItem.hasSubmenu) {
      theMenu = parentItem.submenu;
    } else {
      theMenu = [[NSMenu alloc] init];
      [theMenu setAutoenablesItems:NO];
      [parentItem setSubmenu:theMenu];
    }
  }
  
  NSMenuItem *menuItem;
  menuItem = find_menu_item(theMenu, item->menuId);
  if (menuItem == NULL) {
    menuItem = [theMenu addItemWithTitle:item->title
                               action:@selector(menuHandler:)
                        keyEquivalent:@""];
    [menuItem setRepresentedObject:item->menuId];
  }
  [menuItem setTitle:item->title];
  [menuItem setTag:[item->menuId integerValue]];
  [menuItem setTarget:self];
  [menuItem setToolTip:item->tooltip];
  if (item->disabled == 1) {
    menuItem.enabled = FALSE;
  } else {
    menuItem.enabled = TRUE;
  }
  if (item->checked == 1) {
    menuItem.state = NSControlStateValueOn;
  } else {
    menuItem.state = NSControlStateValueOff;
  }
}

NSMenuItem *find_menu_item(NSMenu *ourMenu, NSNumber *menuId) {
  NSMenuItem *foundItem = [ourMenu itemWithTag:[menuId integerValue]];
  if (foundItem != NULL) {
    return foundItem;
  }
  NSArray *menu_items = ourMenu.itemArray;
  int i;
  for (i = 0; i < [menu_items count]; i++) {
    NSMenuItem *i_item = [menu_items objectAtIndex:i];
    if (i_item.hasSubmenu) {
      foundItem = find_menu_item(i_item.submenu, menuId);
      if (foundItem != NULL) {
        return foundItem;
      }
    }
  }

  return NULL;
};

- (void) add_separator:(NSNumber*) menuId
{
  [menu addItem: [NSMenuItem separatorItem]];
}

- (void) hide_menu_item:(NSNumber*) menuId
{
  NSMenuItem* menuItem = find_menu_item(menu, menuId);
  if (menuItem != NULL) {
    [menuItem setHidden:TRUE];
  }
}

- (void) setMenuItemIcon:(NSArray*)imageAndMenuId {
  NSImage* image = [imageAndMenuId objectAtIndex:0];
  NSNumber* menuId = [imageAndMenuId objectAtIndex:1];

  NSMenuItem* menuItem;
  menuItem = find_menu_item(menu, menuId);
  if (menuItem == NULL) {
    return;
  }
  menuItem.image = image;
}

- (void) show_menu_item:(NSNumber*) menuId
{
  NSMenuItem* menuItem = find_menu_item(menu, menuId);
  if (menuItem != NULL) {
    [menuItem setHidden:FALSE];
  }
}

- (void) quit
{
  [NSApp terminate:self];
}

@end

void registerSystray(void) {
  AppDelegate *delegate = [[AppDelegate alloc] init];
  [[NSApplication sharedApplication] setDelegate:delegate];
  // A workaround to avoid crashing on macOS versions before Catalina. Somehow
  // SIGSEGV would happen inside AppKit if [NSApp run] is called from a
  // different function, even if that function is called right after this.
  if (floor(NSAppKitVersionNumber) <= /*NSAppKitVersionNumber10_14*/ 1671){
    [NSApp run];
  }
}

int nativeLoop(void) {
  if (floor(NSAppKitVersionNumber) > /*NSAppKitVersionNumber10_14*/ 1671){
    [NSApp run];
  }
  return EXIT_SUCCESS;
}

void runInMainThread(SEL method, id object) {
  [(AppDelegate*)[NSApp delegate]
    performSelectorOnMainThread:method
                     withObject:object
                  waitUntilDone: YES];
}

// Menu-bar icon height in points; user-adjustable via setIconHeight. The Go
// side sets it on launch; 19 is the default.
static CGFloat gIconHeight = 19;

void setIconHeight(double h) {
  if (h > 0) {
    gIconHeight = (CGFloat)h;
  }
}

// These entry points are called from Go goroutines, which run on OS threads
// that have no autorelease pool in place. Without an explicit @autoreleasepool
// every autoreleased Cocoa temporary created here (NSData copies of the icon
// bytes, NSImage bitmap reps, NSNumber/NSString) leaks for the life of the
// process. setIcon is the worst offender: the blink loop calls it twice a
// second whenever the app is in an error/429 state, so the leaked NSData/NSImage
// temporaries climb into the gigabytes over a long session. Wrapping each body
// drains those temporaries as soon as the call returns.
void setIcon(const char* iconBytes, int length, bool template) {
  @autoreleasepool {
    NSData* buffer = [NSData dataWithBytes: iconBytes length:length];
    NSImage *image = [[NSImage alloc] initWithData:buffer];
    NSSize imageSize = [image size];
    CGFloat height = gIconHeight;
    CGFloat width = height;
    if (imageSize.height > 0) {
      width = MAX(height, height * imageSize.width / imageSize.height);
    }
    [image setSize:NSMakeSize(width, height)];
    image.template = template;
    runInMainThread(@selector(setIcon:), (id)image);
  }
}

// showSparkOverlay draws image bytes in a width x height point window centred on
// the menu-bar icon, above the menu bar. Call hideSparkOverlay when the
// animation is over — the window stays up until then.
void showSparkOverlay(const char* iconBytes, int length, double width, double height) {
  @autoreleasepool {
    NSData* buffer = [NSData dataWithBytes: iconBytes length:length];
    NSImage *image = [[NSImage alloc] initWithData:buffer];
    if (image == nil) {
      return;
    }
    runInMainThread(@selector(showSpark:),
                    (id)@[image, [NSNumber numberWithDouble:width], [NSNumber numberWithDouble:height]]);
  }
}

// mainScreenHeight is the height in points of the screen holding the menu bar,
// so callers can size an overlay against it.
double mainScreenHeight(void) {
  __block double h = 0;
  void (^read)(void) = ^{
    NSScreen *screen = [[NSScreen screens] firstObject];
    if (screen != nil) {
      h = screen.frame.size.height;
    }
  };
  if ([NSThread isMainThread]) {
    read();
  } else {
    dispatch_sync(dispatch_get_main_queue(), read);
  }
  return h;
}

void hideSparkOverlay(void) {
  @autoreleasepool {
    runInMainThread(@selector(hideSpark:), nil);
  }
}

void setMenuItemIcon(const char* iconBytes, int length, int menuId, bool template) {
  setMenuItemIconSize(iconBytes, length, menuId, 16, 16, template);
}

void setMenuItemIconSize(const char* iconBytes, int length, int menuId, double width, double height, bool template) {
  @autoreleasepool {
    NSData* buffer = [NSData dataWithBytes: iconBytes length:length];
    NSImage *image = [[NSImage alloc] initWithData:buffer];
    CGFloat w = width > 0 ? (CGFloat)width : 16;
    CGFloat h = height > 0 ? (CGFloat)height : 16;
    [image setSize:NSMakeSize(w, h)];
    image.template = template;
    NSNumber *mId = [NSNumber numberWithInt:menuId];
    runInMainThread(@selector(setMenuItemIcon:), @[image, (id)mId]);
  }
}

void setTitle(char* ctitle) {
  @autoreleasepool {
    NSString* title = [[NSString alloc] initWithCString:ctitle
                                               encoding:NSUTF8StringEncoding];
    free(ctitle);
    runInMainThread(@selector(setTitle:), (id)title);
  }
}

void setTooltip(char* ctooltip) {
  @autoreleasepool {
    NSString* tooltip = [[NSString alloc] initWithCString:ctooltip
                                                 encoding:NSUTF8StringEncoding];
    free(ctooltip);
    runInMainThread(@selector(setTooltip:), (id)tooltip);
  }
}

void add_or_update_menu_item(int menuId, int parentMenuId, char* title, char* tooltip, short disabled, short checked, short isCheckable) {
  @autoreleasepool {
    MenuItem* item = [[MenuItem alloc] initWithId: menuId withParentMenuId: parentMenuId withTitle: title withTooltip: tooltip withDisabled: disabled withChecked: checked];
    free(title);
    free(tooltip);
    runInMainThread(@selector(add_or_update_menu_item:), (id)item);
  }
}

void add_separator(int menuId) {
  @autoreleasepool {
    NSNumber *mId = [NSNumber numberWithInt:menuId];
    runInMainThread(@selector(add_separator:), (id)mId);
  }
}

void hide_menu_item(int menuId) {
  @autoreleasepool {
    NSNumber *mId = [NSNumber numberWithInt:menuId];
    runInMainThread(@selector(hide_menu_item:), (id)mId);
  }
}

void show_menu_item(int menuId) {
  @autoreleasepool {
    NSNumber *mId = [NSNumber numberWithInt:menuId];
    runInMainThread(@selector(show_menu_item:), (id)mId);
  }
}

void quit() {
  @autoreleasepool {
    runInMainThread(@selector(quit), nil);
  }
}
