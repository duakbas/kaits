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
    // keep the focused row in view on the tiny screen
    items[i].scrollIntoView({ block: "nearest" });
    focusIndex = i;
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

  document.addEventListener("keydown", function (e) {
    switch (e.key) {
      case "ArrowUp":
        if (screen.onUp) return screen.onUp(e);
        if (moveFocus(-1)) e.preventDefault();
        break;
      case "ArrowDown":
        if (screen.onDown) return screen.onDown(e);
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
      case "EndCall":
      case "Escape":
        if (screen.onBack) { screen.onBack(e); e.preventDefault(); }
        break;
    }
  });

  window.Nav = {
    setScreen: function (handlers) {
      screen = {
        onUp: null, onDown: null, onLeft: null, onRight: null,
        onEnter: null, onSoftLeft: null, onSoftRight: null, onBack: null,
        list: null
      };
      for (var k in handlers) screen[k] = handlers[k];
      focusIndex = -1;
      if (screen.list) setFocus(0);
    },
    refreshFocus: function () { setFocus(focusIndex < 0 ? 0 : focusIndex); },
    focusedEl: currentFocusEl,
    // let a screen set the softkey labels shown in the bottom bar
    setSoftkeys: function (left, center, right) {
      document.getElementById("sk-left").textContent = left || "";
      document.getElementById("sk-center").textContent = center || "";
      document.getElementById("sk-right").textContent = right || "";
    }
  };
})();
