// The menu bar the webview binding does not create.
//
// webview_go builds an NSWindow and an NSApplication and never touches
// NSApp.mainMenu, so a howl window ships with zero NSMenu. That is survivable
// in a dev loop and wrong in an application: on macOS the standard key
// equivalents are not window behaviour, they are *menu item* behaviour. With no
// menu there is no ⌘Q, no ⌘W, no ⌘M, and — the one that gets reported as "the
// text fields are broken" — no ⌘C, ⌘V, ⌘X or ⌘A anywhere in the page, because
// WKWebView implements copy:/paste: on the responder chain and nothing is
// sending them.
//
// So the items are not decoration. Each one is a keystroke that otherwise does
// nothing at all.

#import <Cocoa/Cocoa.h>

static NSMenuItem *item(NSMenu *menu, NSString *title, SEL action, NSString *key) {
  return [menu addItemWithTitle:title action:action keyEquivalent:key];
}

int howl_install_menu(const char *cName) {
  @autoreleasepool {
    NSString *name = [NSString stringWithUTF8String:cName];
    NSApplication *app = [NSApplication sharedApplication];

    // No "leave an existing menu alone" guard here, and that is deliberate.
    // A bundled app is handed a one-item mainMenu by AppKit before any of our
    // code runs, so a guard on numberOfItems fires every time and the menu
    // silently reverts to that single stub — which is why the first version of
    // this worked when the binary was run bare and did nothing at all once it
    // was inside a .app. Opting out is Options.NoMenu, which is checked before
    // this is ever called.

    NSMenu *bar = [[NSMenu alloc] init];

    NSMenu *appMenu = [[NSMenu alloc] initWithTitle:name];
    item(appMenu, [@"About " stringByAppendingString:name],
         @selector(orderFrontStandardAboutPanel:), @"");
    [appMenu addItem:[NSMenuItem separatorItem]];
    item(appMenu, [@"Hide " stringByAppendingString:name], @selector(hide:), @"h");
    NSMenuItem *others = item(appMenu, @"Hide Others",
                              @selector(hideOtherApplications:), @"h");
    others.keyEquivalentModifierMask = NSEventModifierFlagCommand | NSEventModifierFlagOption;
    item(appMenu, @"Show All", @selector(unhideAllApplications:), @"");
    [appMenu addItem:[NSMenuItem separatorItem]];
    item(appMenu, [@"Quit " stringByAppendingString:name], @selector(terminate:), @"q");

    NSMenu *editMenu = [[NSMenu alloc] initWithTitle:@"Edit"];
    item(editMenu, @"Undo", @selector(undo:), @"z");
    NSMenuItem *redo = item(editMenu, @"Redo", @selector(redo:), @"z");
    redo.keyEquivalentModifierMask = NSEventModifierFlagCommand | NSEventModifierFlagShift;
    [editMenu addItem:[NSMenuItem separatorItem]];
    item(editMenu, @"Cut", @selector(cut:), @"x");
    item(editMenu, @"Copy", @selector(copy:), @"c");
    item(editMenu, @"Paste", @selector(paste:), @"v");
    item(editMenu, @"Select All", @selector(selectAll:), @"a");

    NSMenu *windowMenu = [[NSMenu alloc] initWithTitle:@"Window"];
    item(windowMenu, @"Minimize", @selector(performMiniaturize:), @"m");
    item(windowMenu, @"Zoom", @selector(performZoom:), @"");
    [windowMenu addItem:[NSMenuItem separatorItem]];
    item(windowMenu, @"Close", @selector(performClose:), @"w");

    // The bar shows the *holder item's* title, not the submenu's, and the
    // first one is relabelled with the process name by AppKit whatever it
    // says. Leaving the titles empty is why the first attempt produced a menu
    // bar containing only the app menu: Edit and Window were there, zero
    // pixels wide, and absent from the accessibility tree too.
    NSMenu *menus[] = {appMenu, editMenu, windowMenu};
    for (int i = 0; i < 3; i++) {
      NSMenuItem *holder = [[NSMenuItem alloc] initWithTitle:menus[i].title
                                                      action:nil
                                               keyEquivalent:@""];
      holder.submenu = menus[i];
      [bar addItem:holder];
    }

    app.mainMenu = bar;
    // Assigned so the Window menu tracks the open windows itself. Without it
    // the menu is three static items and never lists a window to switch to.
    app.windowsMenu = windowMenu;
    return (int)app.mainMenu.numberOfItems;
  }
}
