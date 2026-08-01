// nav.js — KaiOS hardware key handling. Feature phones drive everything from
// a D-pad (Up/Down/Left/Right + Enter) and two softkeys (left/right) plus a
// Backspace/End key. There is no touch. This module:
//   - tracks a "focused" element within the current screen and moves focus
//     with Up/Down
//   - fires Enter on the focused element
//   - routes the two softkeys and Backspace to the active screen's handlers
//
// Screens register a keymap via Nav.setScreen({...}).

(function () {
  // The active screen handler set. Each is optional.
  var screen = {
    onUp: null, onDown: null, onLeft: null, onRight: null,
    onEnter: null, onSoftLeft: null, onSoftRight: null, onBack: null,
    onFocusChange: null,
    // container element whose [nav] children are focusable, if using
    // built-in list focus:
    list: null
  };

  var focusIndex = -1;

  function focusables() {
    if (!screen.list) return [];
    return Array.prototype.slice.call(
      screen.list.querySelectorAll("[data-nav]")
    );
  }

  function setFocus(i) {
    var items = focusables();
    if (!items.length) { focusIndex = -1; return; }
    if (i < 0) i = 0;
    if (i >= items.length) i = items.length - 1;
    items.forEach(function (el) { el.classList.remove("focused"); });
    items[i].classList.add("focused");
    // Tell the app where focus landed. Reading it back out of the DOM later is
    // unreliable — a rebuild can leave a moment where the answer is "nowhere",
    // and a restore based on that puts focus back at the top.
    if (typeof screen.onFocusChange === "function") {
      try { screen.onFocusChange(items[i]); } catch (e) {}
    }
    // Record the index BEFORE scrolling. This line used to come last, and the
    // scroll below used to throw on Gecko 48 — so focusIndex never updated and
    // every later keypress recomputed from a stale value. The highlight moved
    // once and then appeared frozen, which is the bug that ate most of a day.
    focusIndex = i;
    ensureVisible(items[i]);
  }

  // "nearest" scrolling, done by hand.
  //
  // scrollIntoView({block: "nearest"}) throws a TypeError on Gecko 48: that
  // value for `block` didn't exist yet. Plain scrollIntoView() would work but
  // always jumps the row to the top of the view, which makes a list lurch on
  // every keypress. Comparing rectangles gives the behaviour we wanted with an
  // API this engine actually has.
  function ensureVisible(el) {
    if (!el || !el.getBoundingClientRect) return;
    var box = screen.list;
    try {
      if (!box || !box.getBoundingClientRect) { el.scrollIntoView(); return; }
      var r = el.getBoundingClientRect();
      var c = box.getBoundingClientRect();
      if (r.top < c.top) box.scrollTop -= (c.top - r.top);
      else if (r.bottom > c.bottom) box.scrollTop += (r.bottom - c.bottom);
    } catch (e) {
      // Never let a scroll failure break focus again.
    }
  }

  function moveFocus(delta) {
    var items = focusables();
    if (!items.length) return false;
    setFocus(focusIndex < 0 ? 0 : focusIndex + delta);
    return true;
  }

  function currentFocusEl() {
    var items = focusables();
    return focusIndex >= 0 ? items[focusIndex] : null;
  }

  // True when the user is typing into a field, so key handling should leave
  // text editing alone.
  function isEmptyField() {
    var a = document.activeElement;
    return !!a && typeof a.value === "string" && a.value.length === 0;
  }

  function isEditing() {
    var a = document.activeElement;
    if (!a) return false;
    var tag = (a.tagName || "").toLowerCase();
    return tag === "input" || tag === "textarea" || a.isContentEditable;
  }

  // Every key the page receives, newest last. On the phone there is no console,
  // so this is the only way to tell "the key never arrived" from "the key
  // arrived and nothing handled it" — which look identical and have completely
  // different causes.
  var recent = [];
  function note(key, handled) {
    recent.push((handled ? "" : "!") + key);
    if (recent.length > 6) recent.shift();
    if (window.CONFIG && window.CONFIG.DEBUG_KEYS && window.Nav && Nav.onKeyLog) {
      Nav.onKeyLog(recent.join(" "));
    }
  }

  document.addEventListener("keydown", function (e) {
    note(e.key, true);
    switch (e.key) {
      // A screen's own onUp/onDown wins, EXCEPT when it returns false — that's
      // the handler saying "not mine, do the normal thing". Without that escape
      // hatch a screen that only wants to special-case the top of the list has
      // to reimplement focus movement, and if it forgets, the list simply stops
      // moving in that direction.
      case "ArrowUp":
        if (screen.onUp && screen.onUp(e) !== false) return;
        if (moveFocus(-1)) e.preventDefault();
        break;
      case "ArrowDown":
        if (screen.onDown && screen.onDown(e) !== false) return;
        if (moveFocus(1)) e.preventDefault();
        break;
      case "ArrowLeft":
        if (screen.onLeft) screen.onLeft(e);
        break;
      case "ArrowRight":
        if (screen.onRight) screen.onRight(e);
        break;
      case "Enter":
        if (screen.onEnter) return screen.onEnter(e, currentFocusEl());
        var el = currentFocusEl();
        if (el) { el.click(); e.preventDefault(); }
        break;
      // KaiOS softkeys arrive as SoftLeft / SoftRight. Some emulators send
      // these as F1/F2, so accept both.
      case "SoftLeft":
      case "F1":
        if (screen.onSoftLeft) { screen.onSoftLeft(e); e.preventDefault(); }
        break;
      case "SoftRight":
      case "F2":
        if (screen.onSoftRight) { screen.onSoftRight(e); e.preventDefault(); }
        break;
      // Back / End / Escape
      case "Backspace":
        // While a text field has focus, Backspace has to delete a character —
        // stealing it for "back" makes the composer and the contact-name
        // prompt impossible to correct. But an EMPTY field has nothing to
        // delete, and the End key can't be intercepted on KaiOS (the system
        // closes the app), so without this the search screen has no way out.
        if (isEditing() && !isEmptyField()) break;
        if (screen.onBack) { screen.onBack(e); e.preventDefault(); }
        break;
      // On real hardware the red key is the back/exit key, and which name Gecko
      // gives it varies by device and build — "EndCall" on some, "Escape" or
      // "GoBack" on others. Accept them all; preventDefault also stops the
      // system from treating it as "close the app".
      case "EndCall":
      case "Escape":
      case "GoBack":
      case "BrowserBack":
        if (screen.onBack) { screen.onBack(e); e.preventDefault(); }
        break;
      default:
        // Which key names a given phone actually emits is the sort of thing
        // only the device can tell you. Set DEBUG_KEYS in config.js and press
        // the key to find out, instead of guessing at a keymap.
        recent[recent.length - 1] = "!" + e.key;   // mark it unhandled
        if (window.CONFIG && window.CONFIG.DEBUG_KEYS) {
          console.log("nav: unhandled key", JSON.stringify(e.key), "code", e.code);
          if (window.Nav && Nav.onKeyLog) Nav.onKeyLog(recent.join(" "));
        }
    }
  });

  // Clicking the on-screen softkey bar does what the hardware key does.
  //
  // There is no touch on KaiOS, so this is dead weight on the phone — but on a
  // desktop it removes a whole class of wasted time: macOS takes F1 and F2 for
  // brightness, some browsers claim F1 for help, and a softkey that silently
  // never fires looks exactly like a broken feature.
  function wireSoftkeyClicks() {
    var map = [
      ["sk-left", function (e) { if (screen.onSoftLeft) screen.onSoftLeft(e); }],
      ["sk-center", function (e) {
        if (screen.onEnter) screen.onEnter(e, currentFocusEl());
        else { var el = currentFocusEl(); if (el) el.click(); }
      }],
      ["sk-right", function (e) { if (screen.onSoftRight) screen.onSoftRight(e); }]
    ];
    map.forEach(function (pair) {
      var el = document.getElementById(pair[0]);
      if (el && el.addEventListener) el.addEventListener("click", pair[1]);
    });
  }
  wireSoftkeyClicks();

  window.Nav = {
    // Set by the app to display the key log; "!" marks a key nothing handled.
    onKeyLog: null,
    recentKeys: function () { return recent.join(" "); },
    setScreen: function (handlers) {
      screen = {
        onUp: null, onDown: null, onLeft: null, onRight: null,
        onEnter: null, onSoftLeft: null, onSoftRight: null, onBack: null,
        onFocusChange: null,
        list: null
      };
      for (var k in handlers) screen[k] = handlers[k];
      focusIndex = -1;
      if (screen.list) setFocus(0);
    },
    refreshFocus: function () { setFocus(focusIndex < 0 ? 0 : focusIndex); },
    // Move the highlight to a known row, for callers that rebuilt the list and
    // know where the selection belongs.
    focusIndexAt: function (i) { setFocus(i); },
    focusedEl: currentFocusEl,
    // let a screen set the softkey labels shown in the bottom bar
    setSoftkeys: function (left, center, right) {
      document.getElementById("sk-left").textContent = left || "";
      document.getElementById("sk-center").textContent = center || "";
      document.getElementById("sk-right").textContent = right || "";
    }
  };
})();
