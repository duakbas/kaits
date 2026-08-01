// app.js — screen logic. Three screens for now: chat list, a thread view, and
// an incoming-call screen. State is kept in plain objects (no framework — this
// is Gecko 48, keep it light).

(function () {
  var W = window.Wire;

  // ---- in-memory state ----
  var chats = {};        // jid -> {jid,name,ts,preview,unread}
  var threads = {};      // jid -> [msg,...]
  var currentJID = null; // open thread, or null on list screen
  var activeCall = null; // {callid, from, name} while ringing

  // thread interaction state
  var selectMode = false; // true when a bubble is focused (vs the composer)
  var selectIdx = -1;     // index into the current thread's messages
  var selectMsgID = null; // ...and which message that is, so it survives paging
  var loadingOlder = false;   // an older-history request is in flight
  var pendingOlderFor = null; // which chat that request was for
  var prependAnchor = null;   // scroll geometry captured before older msgs land
  var replyingTo = null;  // {msgid, name, text} when composing a reply
  var privateReply = false; // reply goes to the sender's DM, not this chat
  var menuOpen = false;   // action menu overlay visible
  var forceScrollBottom = false; // force thread to bottom on next render (open)
  var pendingProfile = null; // what to do with the next "profile" frame
  var profileData = null;    // ProfileData currently on the info screen
  var profileBack = null;    // where Back goes from the info screen

  // ---- element refs ----
  var elList = document.getElementById("chatlist");
  var elThread = document.getElementById("thread");
  var elThreadMsgs = document.getElementById("thread-msgs");
  var elThreadTitle = document.getElementById("thread-title");
  var elThreadAvatar = document.getElementById("thread-avatar");
  var elThreadName = document.getElementById("thread-name");
  var elThreadSub = document.getElementById("thread-sub");
  var elInput = document.getElementById("composer");
  var elAttach = document.getElementById("attach-btn");
  var elNotifBar = document.getElementById("notif-bar");
  var elNotifTitle = document.getElementById("notif-title");
  var elNotifBody = document.getElementById("notif-body");
  var elReplyBar = document.getElementById("reply-bar");
  var elReplyBarText = document.getElementById("reply-bar-text");
  var elMenu = document.getElementById("action-menu");
  // The scrolling element, not the overlay: Nav scrolls whatever it is
  // given as the list, and the overlay itself never scrolls.
  var elActionBox = document.getElementById("action-box");
  var elActionList = document.getElementById("action-list");
  var elCall = document.getElementById("call");
  var elCallName = document.getElementById("call-name");
  var elStatus = document.getElementById("status");
  var elProfile = document.getElementById("profile");
  var elProfileBody = document.getElementById("profile-body");
  var elChatMenu = document.getElementById("chat-menu");
  var elChatMenuList = document.getElementById("chat-menu-list");
  var elChatMenuTitle = document.getElementById("chat-menu-title");
  var elPrompt = document.getElementById("prompt");
  var elPromptTitle = document.getElementById("prompt-title");
  var elPromptInput = document.getElementById("prompt-input");
  var elPromptHint = document.getElementById("prompt-hint");
  var elViewer = document.getElementById("viewer");
  var elViewerImg = document.getElementById("viewer-img");
  var elViewerCap = document.getElementById("viewer-caption");
  var elSearch = document.getElementById("search");
  var elSetup = document.getElementById("setup");
  var elSetupHost = document.getElementById("setup-host");
  var elSetupToken = document.getElementById("setup-token");
  var elSetupPreview = document.getElementById("setup-preview");
  var elFocusSink = document.getElementById("focus-sink");
  var elSetupDiag = document.getElementById("setup-diag");
  var elKeyLog = document.getElementById("keylog");

  // No console exists on the phone, so an exception is completely silent — the
  // app just stops doing something and there is nothing to read. Errors go to
  // the same strip the key log uses, whether or not DEBUG_KEYS is on.
  // Errors sit over the softkeys, so they clear themselves after a while
  // rather than staying until the app restarts. Long enough to read and
  // photograph, short enough not to become furniture.
  var ERROR_HOLD_MS = 20000;
  var errorTimer = null;
  var keyLogOn = !!(window.CONFIG && CONFIG.DEBUG_KEYS);

  function showError(msg) {
    if (!elKeyLog) return;
    elKeyLog.hidden = false;
    elKeyLog.style.color = "#e5484d";
    elKeyLog.textContent = String(msg).slice(0, 120);
    if (errorTimer) clearTimeout(errorTimer);
    errorTimer = setTimeout(function () {
      errorTimer = null;
      elKeyLog.style.color = "";
      // Hand the strip back to the key log if it's on, otherwise hide it.
      if (keyLogOn) elKeyLog.textContent = Nav.recentKeys ? Nav.recentKeys() : "";
      else { elKeyLog.textContent = ""; elKeyLog.hidden = true; }
    }, ERROR_HOLD_MS);
  }

  window.onerror = function (msg, src, line) {
    showError(msg + " @" + String(src).split("/").pop() + ":" + line);
    return false;   // keep the default logging too
  };

  // The key log shows every key the page sees; "!" means nothing handled it,
  // and nothing appearing at all means the key never reached us. It's a
  // debugging tool, so it's off unless asked for. Pressing *
  // three times in a row toggles it — a sequence you can't hit by accident on
  // a D-pad, and one that works with no console and no way to edit config on
  // the phone. Errors are separate and always shown.
  var starRun = 0;

  if (elKeyLog) {
    elKeyLog.hidden = !keyLogOn;
    if (keyLogOn) elKeyLog.textContent = "keys: (press one)";
    Nav.onKeyLog = function (text, key) {
      // A null key means the same press being re-reported, so it neither
      // counts nor breaks the run.
      if (key === "*") starRun++;
      else if (key) starRun = 0;
      if (starRun >= 3) {
        starRun = 0;
        keyLogOn = !keyLogOn;
        elKeyLog.hidden = !keyLogOn;
        elKeyLog.style.color = "";          // clear any error colour
        toast(keyLogOn ? "Key log on" : "Key log off");
      }
      if (keyLogOn) {
        elKeyLog.style.color = "";
        elKeyLog.textContent = text;
      }
    };
  }
  var elSearchInput = document.getElementById("search-input");
  var elSearchResults = document.getElementById("search-results");

  // Every screen must be listed here. A screen that isn't gets left visible on
  // top of whatever replaced it — which is how the setup form ended up floating
  // over the chat list, stealing the keypad because its input still had focus.
  var SCREENS = [elList, elThread, elCall, elProfile, elSearch, elSetup];

  // KaiOS keeps a status bar above the app, so the usable height is less than
  // the screen's 320. Laying out against 100% overshoots by that much and the
  // message list runs off the bottom. window.innerHeight is the real number.
  var elApp = document.getElementById("app");
  function fitViewport() {
    if (!elApp || !window.innerHeight) return;
    elApp.style.height = window.innerHeight + "px";
  }
  fitViewport();
  window.addEventListener("resize", fitViewport);
  window.addEventListener("orientationchange", fitViewport);

  function show(el) {
    SCREENS.forEach(function (s) { if (s) s.hidden = true; });
    el.hidden = false;
    releaseInput(el);
  }

  // releaseInput gets the keypad back from a text field.
  //
  // blur() alone is not enough on KaiOS: the input method holds its editing
  // session open, so the keypad keeps feeding a textbox that is no longer on
  // screen and the D-pad does nothing. Moving focus onto a non-input element
  // is what actually ends the session.
  //
  // Skipped when the incoming screen owns the focused element — the thread
  // wants its composer focused.
  function releaseInput(el) {
    var a = document.activeElement;
    if (!a || a === document.body) return;
    if (el && el.contains && el.contains(a)) return;
    try { a.blur(); } catch (e) {}
    // Park focus on the SCREEN, not on an off-screen sink. Focusing an
    // invisible one-pixel div ended the editing session but left the D-pad
    // going nowhere — keys worked while an input held focus and died the
    // moment focus moved off it. A visible, focusable container keeps the app
    // in a state where KaiOS still routes keys to the page.
    if (el && el.focus) {
      try { el.focus(); return; } catch (e) {}
    }
    if (elFocusSink) { try { elFocusSink.focus(); } catch (e) {} }
  }

  // ---------- chat list screen ----------
  // showArchived is a separate view, entered by pressing Up from the topmost
  // chat — the same gesture WhatsApp uses, and it keeps archived chats out of
  // the way without spending a screen on them.
  var showArchived = false;

  function archivedCount() {
    var n = 0;
    for (var k in chats) if (chats[k] && chats[k].archived) n++;
    return n;
  }

  // Which chat the highlight is on, so a rebuild can put it back. Anchoring on
  // the JID rather than the row index is the same lesson the message selection
  // taught: the list re-sorts whenever a message arrives, so an index points at
  // a different conversation a moment later.
  // Where the highlight belongs, updated as the user moves rather than read
  // back from the DOM afterwards.
  var listFocusKey = "";

  function rowKey(el) {
    if (!el || !el.getAttribute) return "";
    if (el.getAttribute("data-settings")) return "\u0000settings";
    if (el.getAttribute("data-archived")) return "\u0000archived";
    return el.getAttribute("data-jid") || "";
  }

  function restoreChatFocus(key) {
    if (elList.hidden) return;
    var rows = elList.querySelectorAll("[data-nav]");
    if (!rows.length) return;
    var want = -1;
    if (key) {
      for (var i = 0; i < rows.length; i++) {
        var r = rows[i];
        var id = r.getAttribute("data-settings") ? "\u0000settings"
               : r.getAttribute("data-archived") ? "\u0000archived"
               : r.getAttribute("data-jid");
        if (id === key) { want = i; break; }
      }
    }
    Nav.focusIndexAt(want >= 0 ? want : 0);
  }

  function renderChatList() {
    // Remember before the rebuild destroys the rows.
    var keepFocus = listFocusKey;
    var arr = Object.keys(chats).map(function (j) { return chats[j]; })
      .filter(function (c) { return !!c.archived === showArchived; });
    arr.sort(function (a, b) {
      if (!!a.pinned !== !!b.pinned) return a.pinned ? -1 : 1;
      return (b.ts || 0) - (a.ts || 0);
    });

    elList.innerHTML = "";

    // The archived entry only appears once you've pressed Up past the top, so
    // it costs nothing until wanted.
    if (!showArchived && archivedRevealed && archivedCount()) {
      var arch = document.createElement("div");
      arch.className = "chat-row archived-entry";
      arch.setAttribute("data-nav", "");
      arch.setAttribute("data-archived", "1");
      arch.textContent = "🗄  Archived (" + archivedCount() + ")";
      arch.onclick = openArchived;
      elList.appendChild(arch);
    }
    if (showArchived) {
      var back = document.createElement("div");
      back.className = "chat-row archived-entry";
      back.setAttribute("data-nav", "");
      back.setAttribute("data-archived-back", "1");
      back.textContent = "←  Back to chats";
      back.onclick = closeArchived;
      elList.appendChild(back);
    }
    if (!arr.length) {
      var empty = document.createElement("div");
      empty.className = "empty";
      empty.textContent = "No chats yet. Waiting for messages…";
      elList.appendChild(empty);
    }
    arr.forEach(function (c) {
      var row = document.createElement("div");
      row.className = "chat-row";
      row.setAttribute("data-nav", "");
      row.setAttribute("data-jid", c.jid);
      row.onclick = function () { openThread(c.jid); };

      // avatar: initials immediately; real photo lazy-loaded when the row is
      // visible (via observer below), throttled so we don't fire 400+ requests
      // at once and trip WhatsApp's rate limit.
      var av = document.createElement("div");
      av.className = "avatar";
      av.textContent = initialsFor(c.name || c.jid);
      av.style.background = colorFor(c.name || c.jid);
      av.setAttribute("data-avatar-jid", c.jid); // observer picks this up

      var col = document.createElement("div");
      col.className = "chat-text";
      var name = document.createElement("div");
      name.className = "chat-name";
      name.textContent = (c.pinned ? "📌 " : "") + (c.muted ? "🔇 " : "") +
        (c.archived ? "🗄 " : "") + (c.name || c.jid);
      var prev = document.createElement("div");
      prev.className = "chat-prev";
      var typed = typingLabel(c.jid);
      if (typed) { prev.className += " typing"; prev.textContent = typed; }
      else if (drafts[c.jid]) {
        prev.className += " draft";
        prev.textContent = "Draft: " + drafts[c.jid];
      } else prev.textContent = c.preview || "";
      col.appendChild(name);
      col.appendChild(prev);

      row.appendChild(av);
      row.appendChild(col);
      if (c.unread > 0) {
        var badge = document.createElement("span");
        badge.className = "unread";
        // Past 9 the exact number stops mattering and starts costing width on
        // a 240px screen.
        badge.textContent = c.unread > 9 ? "9+" : String(c.unread);
        row.appendChild(badge);
      }
      elList.appendChild(row);
    });

    // Settings sits at the very bottom, past every chat. A LAN address changes
    // with DHCP, so there has to be a way back to it — but it's needed once in
    // a blue moon, so it costs nothing to reach it by pressing Down a lot.
    // Not while the list is otherwise empty: it would be the only focusable row
    // and would capture the initial focus before any chat exists.
    if (arr.length) {
    var cog = document.createElement("div");
    cog.className = "chat-row archived-entry";
    cog.setAttribute("data-nav", "");
    cog.setAttribute("data-settings", "1");
    cog.textContent = "⚙  Settings";
    cog.onclick = function () { enterSetupScreen(enterListScreen); };
    elList.appendChild(cog);
    }

    scheduleAvatarLoad();
    // Put the highlight back. Without this every incoming message wipes it, and
    // on a busy account that happens faster than you can press a key — which
    // looks exactly like the D-pad being dead.
    restoreChatFocus(keepFocus);
  }

  // ---- lazy, throttled avatar loading ----
  // Only fetch avatars for rows near the viewport, at most a few at a time, so
  // we never hammer the daemon (and WhatsApp) with hundreds of parallel
  // requests — that flood was tripping WhatsApp's profile-picture rate limit.
  var avatarQueue = [];
  var avatarActive = 0;
  var AVATAR_MAX = 3;           // concurrent fetches
  var avatarObserver = null;

  function scheduleAvatarLoad() {
    if (!("IntersectionObserver" in window)) {
      // fallback: no observer (old engines) — load the first ~12 only
      var els = elList.querySelectorAll("[data-avatar-jid]");
      for (var i = 0; i < els.length && i < 12; i++) enqueueAvatar(els[i]);
      return;
    }
    if (avatarObserver) avatarObserver.disconnect();
    avatarObserver = new IntersectionObserver(function (entries) {
      entries.forEach(function (en) {
        if (en.isIntersecting) {
          enqueueAvatar(en.target);
          avatarObserver.unobserve(en.target);
        }
      });
    }, { root: elList, rootMargin: "100px" });
    elList.querySelectorAll("[data-avatar-jid]").forEach(function (el) {
      avatarObserver.observe(el);
    });
  }

  function enqueueAvatar(av) {
    if (av.getAttribute("data-avatar-done")) return;
    av.setAttribute("data-avatar-done", "1");
    avatarQueue.push(av);
    pumpAvatars();
  }

  function pumpAvatars() {
    while (avatarActive < AVATAR_MAX && avatarQueue.length) {
      var av = avatarQueue.shift();
      var jid = av.getAttribute("data-avatar-jid");
      if (!jid) continue;
      avatarActive++;
      var img = new Image();
      img.className = "avatar-img";
      (function (av, img) {
        img.onload = function () {
          av.textContent = ""; av.style.background = "none"; av.appendChild(img);
          avatarActive--; pumpAvatars();
        };
        img.onerror = function () { avatarActive--; pumpAvatars(); };
      })(av, img);
      img.src = mediaBase() + "/avatar/" + encodeURIComponent(jid);
    }
  }

  var archivedRevealed = false; // the archived row is shown after an Up at the top

  function openArchived() {
    showArchived = true;
    archivedRevealed = false;
    enterListScreen();
  }
  function closeArchived() {
    showArchived = false;
    archivedRevealed = false;
    enterListScreen();
  }

  // Shared by Right and the Options softkey, falling back to the first row so
  // an unset focus doesn't silently swallow the keypress.
  function openMenuForFocusedChat() {
    var el = Nav.focusedEl() || elList.querySelector("[data-nav]");
    if (!el) return;
    if (el.getAttribute("data-settings")) { enterSetupScreen(enterListScreen); return; }
    var jid = el.getAttribute("data-jid");
    if (jid) openChatMenu(jid);
  }

  function enterListScreen() {
    stashDraft();
    stopTyping();
    currentJID = null;
    // The list shows every unread chat with a badge, so a saved notification
    // has nothing left to tell you.
    clearNotif();
    show(elList);
    Nav.setScreen({
      list: elList,
      onFocusChange: function (el) { listFocusKey = rowKey(el); },
      onUp: function (e) {
        // At the very top, one more Up reveals the archived entry rather than
        // doing nothing.
        var el = Nav.focusedEl();
        var first = elList.querySelector("[data-nav]");
        if (el && el === first && !showArchived && archivedCount() && !archivedRevealed) {
          archivedRevealed = true;
          renderChatList();
          Nav.refreshFocus();
          e.preventDefault();
          return true;
        }
        return false; // not handled: let Nav move focus up as usual
      },
      onEnter: function (e, el) {
        // Fall back to the first row when nothing is focused. Every action here
        // used to give up silently in that case, which made the whole list look
        // dead while Search — the one handler that needs no focus — kept
        // working. A key that does nothing and says nothing is the worst
        // possible failure to debug on a device with no console.
        el = el || elList.querySelector("[data-nav]");
        if (!el) return;
        if (el.getAttribute("data-archived")) { openArchived(); return; }
        if (el.getAttribute("data-archived-back")) { closeArchived(); return; }
        if (el.getAttribute("data-settings")) { enterSetupScreen(enterListScreen); return; }
        openThread(el.getAttribute("data-jid"));
      },
      onSoftLeft: enterSearchScreen,
      // Right on a chat row opens its pin/mute/archive/delete menu. The right
      // softkey does the same, since Right isn't discoverable on its own.
      onRight: function () { openMenuForFocusedChat(); },
      onSoftRight: function () { openMenuForFocusedChat(); }
    });
    Nav.setSoftkeys("Search", "SELECT", "Options");
    renderChatList();
  }

  // ---------- chat action menu (pin / mute / archive / delete / info) ----------
  // These are real WhatsApp account changes, not local preferences: they sync
  // to the phone and every other linked device. Delete is not undoable, so it
  // asks for confirmation first.
  var chatMenuJID = null;
  var chatMenuReturn = null;  // where Cancel goes: the list, or back into a chat

  // openThreadOptions is the "•••" softkey inside a chat: the same pin / mute /
  // archive / info / delete menu the list offers, but returning to the chat
  // instead of dumping you back on the list.
  function openThreadOptions() {
    if (!currentJID) return;
    openChatMenu(currentJID, returnToThread);
  }

  // returnToThread, not enterComposeMode: "Contact info" swaps to the profile
  // screen, and show() hides every other screen, so coming back has to put the
  // thread on screen again — not merely re-bind the keys.
  function returnToThread() {
    if (!currentJID) { enterListScreen(); return; }
    show(elThread);
    enterComposeMode();
  }

  // returnTo is where Cancel and a completed action go back to. Defaults to the
  // chat list, which is where this menu was originally only ever opened from.
  function openChatMenu(jid, returnTo) {
    if (!jid) return;
    var c = chats[jid] || { jid: jid };
    chatMenuJID = jid;
    chatMenuReturn = returnTo || enterListScreen;
    elChatMenuTitle.textContent = c.name || jid;
    elChatMenuList.innerHTML = "";
    [
      { action: "pin", label: c.pinned ? "Unpin" : "Pin" },
      { action: "mute", label: c.muted ? "Unmute" : "Mute" },
      { action: "archive", label: c.archived ? "Unarchive" : "Archive" },
      { action: "info", label: c.group ? "Group info" : "Contact info" },
      { action: "delete", label: "Delete chat" },
      // Not about this chat, but it's the only menu reachable from the list
      // without scrolling past every conversation — and the settings screen is
      // where the diagnostics live, so it has to be quick to reach.
      { action: "settings", label: "⚙  App settings" }
    ].forEach(function (item) {
      var row = document.createElement("div");
      row.className = "menu-item" + (item.action === "delete" ? " danger" : "");
      row.setAttribute("data-nav", "");
      row.setAttribute("data-action", item.action);
      row.textContent = item.label;
      elChatMenuList.appendChild(row);
    });
    elChatMenu.hidden = false;
    Nav.setScreen({
      list: elChatMenu,
      onEnter: function (e, el) { if (el) runChatAction(el.getAttribute("data-action")); },
      onSoftRight: function () {
        var el = Nav.focusedEl();
        if (el) runChatAction(el.getAttribute("data-action"));
      },
      onSoftLeft: closeChatMenu,
      onBack: closeChatMenu
    });
    Nav.setSoftkeys("Cancel", "", "OK");
  }

  function closeChatMenu() {
    elChatMenu.hidden = true;
    chatMenuJID = null;
    var back = chatMenuReturn || enterListScreen;
    chatMenuReturn = null;
    back();
  }

  function runChatAction(action) {
    var jid = chatMenuJID;
    if (!jid) { closeChatMenu(); return; }
    var c = chats[jid] || {};
    var back = chatMenuReturn || enterListScreen;
    if (action === "settings") {
      elChatMenu.hidden = true;
      chatMenuJID = null;
      chatMenuReturn = null;
      enterSetupScreen(back);
      return;
    }
    if (action === "info") {
      elChatMenu.hidden = true;
      openProfile(jid, back);
      return;
    }
    if (action === "delete") {
      elChatMenu.hidden = true;
      // Deleting always lands on the list: going "back" into a chat that no
      // longer exists would be nonsense.
      confirmPrompt("Delete chat?",
        "Deletes it on WhatsApp and every linked device. Cannot be undone.",
        function () { W.send(W.T.CHATACTION, { chat: jid, action: "delete" }); enterListScreen(); },
        function () { openChatMenu(jid, back); });
      return;
    }
    // pin / mute / archive: flip the current state, and echo it locally so the
    // list updates now. The daemon confirms with a chatupdate, or an error
    // frame plus a fresh chatlist that puts the truth back.
    var on = !c[action === "pin" ? "pinned" : action === "mute" ? "muted" : "archived"];
    W.send(W.T.CHATACTION, { chat: jid, action: action, on: on });
    if (action === "pin") c.pinned = on;
    else if (action === "mute") c.muted = on;
    else { c.archived = on; if (on) c.pinned = false; }
    closeChatMenu();
  }

  // ---------- thread screen ----------
  // The thread has two modes:
  //   compose — composer focused; type + send. Up-arrow enters select mode.
  //   select  — a bubble is focused; Up/Down move between messages, Enter opens
  //             the action menu (Reply/Copy...). Down past the last message, or
  //             Back, returns to compose.
  function openThread(jid) {
    currentJID = jid;
    // Opening the chat that pinged you consumes the notification; opening some
    // other chat leaves it waiting, because it's still unread news.
    if (pendingNotif && pendingNotif.jid === jid) clearNotif();
    else hideNotifBar();
    var c = chats[jid] || { jid: jid, name: jid };
    // Clear optimistically; the daemon confirms with a chatupdate once the read
    // receipt has actually gone out.
    c.unread = 0;
    // Drop any paging state belonging to the chat we just left, or a reply
    // still in flight for it would keep this chat from loading older pages.
    loadingOlder = false;
    pendingOlderFor = null;
    prependAnchor = null;
    setThreadHeader(jid, c.name || jid);
    show(elThread);
    clearReply();
    forceScrollBottom = true;
    // ask the daemon for stored history for this chat (latest page)
    if (!threads[jid] || !threads[jid].loadedHistory) {
      W.send(W.T.GETHISTORY, { jid: jid, before: 0, limit: 40 });
    }
    // Opening a chat is the read signal — tell WhatsApp, so the sender's ticks
    // turn blue. The daemon works out which messages are still unread.
    W.send(W.T.MARKREAD, { jid: jid });
    // WhatsApp only sends presence for people you've subscribed to, and the
    // subscription resets on reconnect — so ask each time the chat opens.
    if (!(chats[jid] && chats[jid].group)) W.send(W.T.WATCH, { jid: jid });
    // Rendering must never prevent the screen from being bound. When something
    // threw in here, openThread stopped half way: the thread was displayed but
    // Nav still had the PREVIOUS screen, so the softkeys kept the old labels,
    // the composer never got focus, and up/down scrolled whatever list the old
    // screen owned. Three symptoms, one abandoned function.
    try {
      renderThread();
    } catch (e) {
      showError("render: " + (e && e.message ? e.message : e));
    }
    try {
      restoreDraft(jid);
    } catch (e) {
      showError("draft: " + (e && e.message ? e.message : e));
    }
    // The share belongs to one chat, so the bar appears in that chat only.
    paintLiveBar();
    enterComposeMode();
  }

  function enterComposeMode() {
    selectMode = false;
    selectIdx = -1;
    selectMsgID = null;
    clearSelection(); // just drop the highlight; no need to rebuild the thread
    Nav.setScreen({
      onUp: enterSelectMode,          // arrow up starts selecting messages
      onLeft: openAttachMenu,         // attachments live off Left from the composer
      onBack: enterListScreen,
      // Left softkey is "check notification" when one is waiting, plain "Back"
      // otherwise. The red hardware key goes back either way, which is what
      // freed this one up.
      onSoftLeft: checkNotif,
      // Right softkey is the options menu, matching the platform convention:
      // the CENTRE key advertises what Enter does, and the right one is "•••".
      // Send moved to the centre label accordingly — Enter already sent, so the
      // old right-softkey Send was the odd one out.
      onSoftRight: openThreadOptions,
      onEnter: sendCurrent
    });
    Nav.setSoftkeys(notifSoftLabel(), "SEND", "•••");
    setTimeout(function () { elInput.focus(); }, 0);
  }

  function enterSelectMode() {
    var msgs = threads[currentJID] || [];
    if (!msgs.length) return;
    elInput.blur();
    selectMode = true;
    setSelect(msgs.length - 1); // start at the newest message
    paintSelection();
    Nav.setScreen({
      onUp: function () { moveSelect(-1); },
      onDown: function () { moveSelect(1); },
      onEnter: enterOnSelected,
      onSoftLeft: enterComposeMode,   // "Cancel" back to typing
      onSoftRight: openActionMenu,    // "Actions"
      onBack: enterComposeMode
    });
    // Centre advertises the Enter action here too; "•••" does the same thing,
    // so the options key means the same in both thread modes.
    Nav.setSoftkeys("Cancel", selectedIsViewable() ? "VIEW" : "ACTIONS", "•••");
  }

  // Enter does the obvious thing for what's selected: a picture opens
  // full-screen, everything else opens the action menu. The viewer already
  // existed but was three keypresses deep behind Actions -> View photo, which
  // is a lot of ceremony for "look at this properly". Actions is still one
  // press away on the right softkey.
  function selectedIsViewable() {
    var msgs = threads[currentJID] || [];
    var m = msgs[selectIdx];
    return !!(m && !m.deleted && m.media &&
      (m.kind === "image" || m.kind === "sticker"));
  }

  function enterOnSelected() {
    var msgs = threads[currentJID] || [];
    var m = msgs[selectIdx];
    if (m && !m.deleted && m.media &&
        (m.kind === "image" || m.kind === "sticker")) {
      openViewer(m);
      return;
    }
    openActionMenu();
  }

  function moveSelect(delta) {
    var msgs = threads[currentJID] || [];
    var next = selectIdx + delta;
    if (next < 0) {
      // At the oldest loaded message. Pull an older page instead of
      // dead-ending here — the scroll listener never fires for D-pad
      // navigation, so without this, moving up simply stops.
      setSelect(0);
      requestOlderHistory();
      paintSelection();
      return;
    }
    if (next >= msgs.length) { enterComposeMode(); return; } // down off the end
    setSelect(next);
    paintSelection();
    // The centre key means something different on a photo, so say which.
    Nav.setSoftkeys("Cancel", selectedIsViewable() ? "VIEW" : "ACTIONS", "•••");
  }

  // paintSelection moves the highlight by editing two class lists.
  //
  // This used to call renderThread(), which threw away every bubble and built
  // them all again for a single D-pad press. On Gecko 48 with a long thread
  // that is slow on its own, but the visible damage came from innerHTML = ""
  // resetting scrollTop to 0: the view snapped to the top of the chat and then
  // scrollIntoView dragged it back. The further into the present you were, the
  // longer that round trip, which is why it got worse the closer you got to
  // today's messages.
  //
  // Falls back to a full render only if the target bubble isn't on screen —
  // which shouldn't happen, since the selection can only point at a message
  // that was rendered.
  function paintSelection() {
    var prev = elThreadMsgs.querySelector(".bubble.selected");
    var next = selectMsgID
      ? elThreadMsgs.querySelector('.bubble[data-msgid="' + cssEscape(selectMsgID) + '"]')
      : null;
    if (selectMsgID && !next) { renderThread(); return; }
    if (prev === next) { if (next) Nav.ensureVisible(next, elThreadMsgs); return; }
    if (prev) prev.className = prev.className.replace(/ ?\bselected\b/, "");
    if (next) {
      next.className += " selected";
      Nav.ensureVisible(next, elThreadMsgs);
    }
  }

  // clearSelection drops the highlight without touching anything else.
  function clearSelection() {
    var prev = elThreadMsgs.querySelector(".bubble.selected");
    if (prev) prev.className = prev.className.replace(/ ?\bselected\b/, "");
  }

  // WhatsApp message ids are [A-Z0-9] hex-ish, but ours also include local
  // echo ids we generate, so quote anything that would break the selector.
  function cssEscape(s) {
    return String(s).replace(/["\\]/g, "\\$&");
  }

  // setSelect moves the selection and remembers WHICH message it is, not just
  // where it sat. Loading older history prepends to the array, shifting every
  // index — anchoring on the id is what stops the selection silently jumping to
  // a different message when a page loads.
  function setSelect(idx) {
    var msgs = threads[currentJID] || [];
    selectIdx = idx;
    selectMsgID = (idx >= 0 && idx < msgs.length) ? msgs[idx].msgid : null;
  }

  // requestOlderHistory asks for the page before the oldest message we hold.
  // Returns true if a request went out. Guards against overlapping requests and
  // against asking forever once the chat's history is exhausted.
  function requestOlderHistory() {
    if (loadingOlder || !currentJID) return false;
    var arr = threads[currentJID] || [];
    if (!arr.length || arr.noMoreHistory) return false;
    var oldestTS = arr[0].ts || 0;
    if (!oldestTS) return false;
    loadingOlder = true;
    pendingOlderFor = currentJID;
    prependAnchor = {
      height: elThreadMsgs.scrollHeight,
      top: elThreadMsgs.scrollTop
    };
    W.send(W.T.GETHISTORY, { jid: currentJID, before: oldestTS, limit: 40 });
    return true;
  }

  if (elAttach) elAttach.onclick = function () { openAttachMenu(); };

  // openAttachMenu offers the attachment kinds. Reuses the chat-menu overlay
  // rather than adding another screen.
  function openAttachMenu() {
    if (!currentJID) return;
    elChatMenuTitle.textContent = "Attach";
    elChatMenuList.innerHTML = "";
    var attachItems = [
      { action: "photo", label: "📷  Photo" },
      { action: "video", label: "🎬  Video" },
      { action: "audio", label: "🎵  Audio" },
      { action: "file", label: "📎  File" }
    ];
    if (haveGeolocation()) {
      attachItems.push({ action: "location", label: "📍  Location" });
      attachItems.push({
        action: "livelocation",
        label: liveShare.active ? "🛑  Stop live location" : "🛰  Live location"
      });
    }
    attachItems.forEach(function (it) {
      var row = document.createElement("div");
      row.className = "menu-item";
      row.setAttribute("data-nav", "");
      row.setAttribute("data-attach", it.action);
      row.textContent = it.label;
      elChatMenuList.appendChild(row);
    });
    elChatMenu.hidden = false;
    var close = function () { elChatMenu.hidden = true; enterComposeMode(); };
    Nav.setScreen({
      list: elChatMenu,
      onEnter: function (e, el) {
        if (!el) return;
        elChatMenu.hidden = true;
        runAttach(el.getAttribute("data-attach"));
        enterComposeMode();
      },
      onSoftLeft: close, onBack: close,
      onSoftRight: function () {
        var el = Nav.focusedEl();
        if (el) {
          elChatMenu.hidden = true;
          runAttach(el.getAttribute("data-attach"));
          enterComposeMode();
        }
      }
    });
    Nav.setSoftkeys("Cancel", "", "OK");
  }

  // ---------- action menu ----------
  function openActionMenu() {
    var msgs = threads[currentJID] || [];
    if (selectIdx < 0 || selectIdx >= msgs.length) return;
    var m = msgs[selectIdx];
    var isGroup = !!(chats[currentJID] && chats[currentJID].group);
    var fromOther = !m.fromme;

    // Build the rows that actually apply to THIS message. "Reply privately"
    // and "Message directly" only make sense for someone else's message in a
    // group; deleting for everyone only works on your own.
    var items = [];
    if (m.kind === "location") items.push({ action: "openmap", label: "Open in maps" });
    if ((m.kind === "image" || m.kind === "sticker") && m.media) {
      items.push({ action: "view", label: "View photo" });
    }
    items.push({ action: "reply", label: "Reply" });
    var mine = myReactionTo(m);
    items.push({ action: "react", label: mine ? "Change reaction " + mine : "React" });
    if (mine) items.push({ action: "unreact", label: "Remove my reaction" });
    var reactionCount = (m.reactions || []).length;
    if (reactionCount) {
      items.push({
        action: "reactions",
        label: "Reactions (" + reactionCount + ")"
      });
    }
    if (isGroup && fromOther) {
      items.push({ action: "replyprivate", label: "Reply privately" });
      items.push({ action: "dm", label: "Message " + shortName(m.sendername) });
    }
    if (fromOther) items.push({ action: "profile", label: "View profile" });
    items.push({ action: "forward", label: "Forward" });
    if (m.fromme) {
      // WhatsApp only accepts an edit for about 15 minutes after sending, so
      // offering it on a week-old message would just produce an error.
      if (m.kind === "text" && !m.deleted && withinEditWindow(m)) {
        items.push({ action: "edit", label: "Edit" });
      }
      items.push({ action: "delete-all", label: "Delete for everyone", danger: true });
    } else if (isGroup) {
      // Revoking someone else's message is an admin power; the daemon checks
      // and reports back rather than us guessing.
      items.push({ action: "delete-all", label: "Delete for everyone", danger: true });
    }
    items.push({ action: "delete-me", label: "Delete for me", danger: true });
    items.push({ action: "copy", label: "Copy text" });

    elActionList.innerHTML = "";
    items.forEach(function (it) {
      var row = document.createElement("div");
      row.className = "menu-item" + (it.danger ? " danger" : "");
      row.setAttribute("data-nav", "");
      row.setAttribute("data-action", it.action);
      row.textContent = it.label;
      elActionList.appendChild(row);
    });

    menuOpen = true;
    elMenu.hidden = false;
    // A previous open may have left the box scrolled down; the new menu starts
    // at its first item, so start the scroll there too.
    if (elActionBox) elActionBox.scrollTop = 0;
    Nav.setScreen({
      list: elActionBox || elMenu,
      onEnter: function (e, el) { if (el) runAction(el.getAttribute("data-action")); },
      onSoftLeft: closeActionMenu,
      onSoftRight: function () {
        var el = Nav.focusedEl();
        if (el) runAction(el.getAttribute("data-action"));
      },
      onBack: closeActionMenu
    });
    Nav.setSoftkeys("Close", "", "OK");
  }

  function closeActionMenu() {
    menuOpen = false;
    elMenu.hidden = true;
    // enterSelectMode jumps to the newest message, which would silently move
    // the user's selection out from under them on cancel — so restore it.
    var keep = selectIdx;
    enterSelectMode();
    setSelect(keep);
    renderThread();
  }

  function runAction(action) {
    var msgs = threads[currentJID] || [];
    var m = msgs[selectIdx];
    if (!m) { closeActionMenu(); return; }
    if (action === "reply") {
      startReply(m);
      menuOpen = false;
      elMenu.hidden = true;
      enterComposeMode();
    } else if (action === "delete-all") {
      menuOpen = false;
      elMenu.hidden = true;
      confirmPrompt("Delete for everyone?",
        m.fromme ? "Removes it for everyone in this chat."
                 : "Only group admins can do this; WhatsApp will refuse otherwise.",
        function () {
          W.send(W.T.DELETE, { chat: currentJID, msgid: m.msgid, scope: "everyone" });
          m.deleted = true;
          forceRerender();
          renderThread();
          enterComposeMode();
        },
        function () { enterComposeMode(); });
    } else if (action === "delete-me") {
      menuOpen = false;
      elMenu.hidden = true;
      confirmPrompt("Delete for me?",
        "Removes it from this app only. Everyone else keeps it, and WhatsApp " +
        "on your phone is unaffected.",
        function () {
          W.send(W.T.DELETE, { chat: currentJID, msgid: m.msgid, scope: "me" });
          removeLocalMessage(currentJID, m.msgid);
          enterComposeMode();
        },
        function () { enterComposeMode(); });
    } else if (action === "edit") {
      menuOpen = false;
      elMenu.hidden = true;
      textPrompt("Edit message", m.text || "",
        "WhatsApp only allows edits for about 15 minutes after sending.",
        function (val) {
          if (val && val !== m.text) {
            W.send(W.T.EDIT, { chat: currentJID, msgid: m.msgid, text: val });
          }
          enterComposeMode();
        },
        function () { enterComposeMode(); });
    } else if (action === "forward") {
      menuOpen = false;
      elMenu.hidden = true;
      openForwardPicker(m);
    } else if (action === "copy") {
      // Gecko 48 has no reliable clipboard API; just drop the text into the
      // composer so the user can resend/edit it. Practical on a feature phone.
      elInput.value = (m.text || "");
      menuOpen = false;
      elMenu.hidden = true;
      enterComposeMode();
    } else if (action === "view") {
      menuOpen = false;
      elMenu.hidden = true;
      openViewer(m);
    } else if (action === "openmap") {
      menuOpen = false;
      elMenu.hidden = true;
      openLocation(m);
      enterSelectMode();
      selectIdx = keepSelection(m);
      renderThread();
    } else if (action === "react") {
      menuOpen = false;
      elMenu.hidden = true;
      promptForReaction(m);
    } else if (action === "unreact") {
      W.send(W.T.SENDREACTION, { chat: currentJID, msgid: m.msgid, emoji: "" });
      menuOpen = false;
      elMenu.hidden = true;
      enterSelectMode();
      selectIdx = keepSelection(m);
      renderThread();
    } else if (action === "reactions") {
      menuOpen = false;
      elMenu.hidden = true;
      openReactionList(m);
    } else if (action === "replyprivate") {
      // Same composer, but the send is flagged private — the daemon redirects
      // it to the sender's DM and keeps the group message as the quote.
      startReply(m);
      privateReply = true;
      elReplyBarText.textContent = "↩ privately to " +
        shortName(m.sendername) + ": " + truncate(replyingTo.text, 30);
      menuOpen = false;
      elMenu.hidden = true;
      enterComposeMode();
    } else if (action === "dm") {
      menuOpen = false;
      elMenu.hidden = true;
      openDirectChat(m);
    } else if (action === "profile") {
      menuOpen = false;
      elMenu.hidden = true;
      // In a group the bubble's sender is a per-group LID, so ask the daemon
      // to resolve the real person from the message id instead of the JID.
      var back = currentJID;
      openProfileForMessage(m, function () { openThread(back); });
    }
  }

  // ---------- drafts ----------
  // Losing a half-typed message when you leave a chat is bad anywhere; with T9
  // it's brutal. Drafts are kept per chat and survive a refresh via
  // localStorage, guarded because it may be missing or full on KaiOS.
  var drafts = {};
  var DRAFT_KEY = "wa_drafts";

  function loadDrafts() {
    try {
      var raw = localStorage.getItem(DRAFT_KEY);
      if (raw) drafts = JSON.parse(raw) || {};
    } catch (e) { drafts = {}; }
  }
  function saveDrafts() {
    try { localStorage.setItem(DRAFT_KEY, JSON.stringify(drafts)); } catch (e) {}
  }
  loadDrafts();

  function stashDraft() {
    if (!currentJID) return;
    var v = elInput.value;
    if (v && v.trim()) drafts[currentJID] = v;
    else delete drafts[currentJID];
    saveDrafts();
  }

  function restoreDraft(jid) {
    elInput.value = drafts[jid] || "";
  }

  function clearDraft(jid) {
    if (drafts[jid]) { delete drafts[jid]; saveDrafts(); }
  }

  // ---------- typing indicators ----------
  // Incoming: chat JID -> {names:{sender:name}, timer}. WhatsApp doesn't always
  // send a "paused" to close a "composing", so each entry self-expires.
  var typing = {};
  var TYPING_TTL = 8000;

  function setTyping(chat, sender, name, composing) {
    var entry = typing[chat] || (typing[chat] = { names: {} });
    if (composing) {
      entry.names[sender] = name || "Someone";
    } else {
      delete entry.names[sender];
    }
    if (entry.timer) clearTimeout(entry.timer);
    entry.timer = setTimeout(function () {
      delete typing[chat];
      refreshTypingUI(chat);
    }, TYPING_TTL);
    refreshTypingUI(chat);
  }

  function typingLabel(chat) {
    var entry = typing[chat];
    if (!entry) return "";
    var names = Object.keys(entry.names).map(function (k) { return entry.names[k]; });
    if (!names.length) return "";
    if (names.length === 1) {
      // In a DM the name is redundant — you know who you're talking to.
      return (chats[chat] && chats[chat].group) ? names[0] + " is typing…" : "typing…";
    }
    return names.length + " people are typing…";
  }

  function refreshTypingUI(chat) {
    if (currentJID === chat) setThreadSub(chat);
    if (!elList.hidden) renderChatList();
  }

  // setThreadSub writes the second header line. Typing outranks presence: it's
  // the more immediate fact, and it gets the accent colour so a live "typing…"
  // reads differently from a stale "last seen".
  function setThreadSub(chat) {
    var typed = typingLabel(chat);
    elThreadSub.className = typed ? "typing" : "";
    elThreadSub.textContent = typed || presenceLabel(chat);
  }

  // setThreadHeader fills the avatar and name when a chat opens. The avatar
  // goes through the same lazy queue as the list rows, so it obeys the same
  // rate limit — profile-picture requests are what tripped WhatsApp before.
  function setThreadHeader(jid, name) {
    elThreadName.textContent = name;
    elThreadAvatar.textContent = initialsFor(name);
    elThreadAvatar.style.background = colorFor(name);
    // Reset the lazy-load bookkeeping, or switching chats keeps the previous
    // contact's photo.
    var old = elThreadAvatar.querySelector("img");
    if (old) elThreadAvatar.removeChild(old);
    elThreadAvatar.removeAttribute("data-avatar-done");
    elThreadAvatar.setAttribute("data-avatar-jid", jid);
    enqueueAvatar(elThreadAvatar);
    setThreadSub(jid);
  }

  // presenceLabel renders "online" or "last seen ...". A zero lastseen means
  // the contact hides it, which is different from "a long time ago" — so we
  // show nothing rather than inventing a date.
  function presenceLabel(chat) {
    var p = presence[chat];
    if (!p) return "";
    if (p.online) return "online";
    if (!p.lastseen) return "";
    var d = new Date(p.lastseen * 1000);
    var days = daysBetween(d, new Date());
    if (days === 0) return "last seen " + timeOf(p.lastseen);
    if (days === 1) return "last seen yesterday";
    return "last seen " + d.getDate() + " " + MONTH_NAMES[d.getMonth()];
  }

  // ---------- outgoing typing ----------
  // Send "composing" while the user types, and "paused" once they stop. Rate
  // limited: WhatsApp wants an occasional refresh, not one per keystroke.
  var typingSentAt = 0;
  var typingStopTimer = null;
  var TYPING_REFRESH = 4000;

  function noteTyping() {
    if (!currentJID) return;
    var now = Date.now();
    if (now - typingSentAt > TYPING_REFRESH) {
      W.send(W.T.TYPING, { chat: currentJID, state: "composing" });
      typingSentAt = now;
    }
    if (typingStopTimer) clearTimeout(typingStopTimer);
    typingStopTimer = setTimeout(stopTyping, 3000);
  }

  function stopTyping() {
    if (typingStopTimer) { clearTimeout(typingStopTimer); typingStopTimer = null; }
    if (!currentJID || !typingSentAt) return;
    W.send(W.T.TYPING, { chat: currentJID, state: "paused" });
    typingSentAt = 0;
  }

  // ---------- time formatting ----------
  // 24-hour clock: unambiguous, and narrower than "10:45 PM" on a 240px screen.
  function timeOf(ts) {
    if (!ts) return "";
    var d = new Date(ts * 1000);
    return pad2(d.getHours()) + ":" + pad2(d.getMinutes());
  }
  function pad2(n) { return n < 10 ? "0" + n : String(n); }

  var DAY_NAMES = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday",
                   "Friday", "Saturday"];
  var MONTH_NAMES = ["Jan", "Feb", "Mar", "Apr", "May", "Jun",
                     "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

  // Date separator label, WhatsApp-style: Today / Yesterday / weekday within the
  // last week / an explicit date beyond that.
  function dayLabel(ts) {
    var d = new Date(ts * 1000);
    var today = new Date();
    var days = daysBetween(d, today);
    if (days === 0) return "Today";
    if (days === 1) return "Yesterday";
    if (days < 7) return DAY_NAMES[d.getDay()];
    var label = d.getDate() + " " + MONTH_NAMES[d.getMonth()];
    if (d.getFullYear() !== today.getFullYear()) label += " " + d.getFullYear();
    return label;
  }

  // Whole days between two dates, ignoring the time of day — comparing
  // timestamps directly would call 23:59 and 00:01 "the same day".
  function daysBetween(a, b) {
    var da = new Date(a.getFullYear(), a.getMonth(), a.getDate());
    var db = new Date(b.getFullYear(), b.getMonth(), b.getDate());
    return Math.round((db - da) / 86400000);
  }

  function sameDay(tsA, tsB) {
    if (!tsA || !tsB) return false;
    return daysBetween(new Date(tsA * 1000), new Date(tsB * 1000)) === 0;
  }

  // ---------- delivery ticks ----------
  // ✓ sent, ✓✓ delivered, ✓✓ (blue) read. Only meaningful on our own messages.
  function tickFor(status) {
    if (status === "read" || status === "played") return { mark: "✓✓", cls: "tick read" };
    if (status === "delivered") return { mark: "✓✓", cls: "tick" };
    return { mark: "✓", cls: "tick" };
  }

  // ---------- reactions ----------
  // Group a message's reactions by emoji, preserving first-seen order so the
  // row doesn't reshuffle as more arrive.
  function groupReactions(list) {
    var order = [];
    var byEmoji = {};
    (list || []).forEach(function (r) {
      if (!r || !r.emoji) return;
      if (!byEmoji[r.emoji]) { byEmoji[r.emoji] = []; order.push(r.emoji); }
      byEmoji[r.emoji].push(r);
    });
    return order.map(function (e) { return { emoji: e, people: byEmoji[e] }; });
  }

  // myReactionTo returns the emoji WE reacted with, or "". The daemon labels our
  // own reaction "You", which is the only marker available client-side.
  function myReactionTo(m) {
    var list = (m && m.reactions) || [];
    for (var i = 0; i < list.length; i++) {
      if (list[i].sendername === "You") return list[i].emoji;
    }
    return "";
  }

  // keepSelection re-finds a message's index after the thread array shifts, and
  // re-anchors the id with it so a later history page can find it again.
  function keepSelection(m) {
    var msgs = threads[currentJID] || [];
    for (var i = 0; i < msgs.length; i++) {
      if (msgs[i].msgid === m.msgid) { setSelect(i); return i; }
    }
    return selectIdx;
  }

  // promptForReaction opens a text field so the PHONE's keyboard provides the
  // emoji, rather than shipping our own picker.
  //
  // KaiOS 2.5 has no API to open the keyboard directly on its emoji panel —
  // there is no inputmode for it, and the panel is reached by the user cycling
  // input modes. So the best available behaviour is: focus a field, which raises
  // the keyboard, and let them switch to emoji. Whatever they enter is sent
  // as-is, which also means a desktop browser can just type or paste one.
  function promptForReaction(m) {
    var mine = myReactionTo(m);
    textPrompt("React", mine,
      "Switch the keyboard to emoji, then pick one. Leave empty to remove.",
      function (val) {
        // Send what was typed, only guarding against a whole sentence being
        // submitted as a "reaction". Deliberately NOT splitting into one
        // character: a single emoji can be many code units (skin tones,
        // variation selectors, ZWJ sequences like 👨‍👩‍👧), and Gecko 48 has no
        // Intl.Segmenter to split them correctly. Truncating would corrupt
        // them, so a generous cap is safer than clever slicing.
        var emoji = (val || "").trim();
        if (emoji.length > 16) emoji = "";
        if (emoji === "" && val && val.trim()) {
          toast("That's too long for a reaction");
        } else {
          W.send(W.T.SENDREACTION, { chat: currentJID, msgid: m.msgid, emoji: emoji });
        }
        enterSelectMode();
        selectIdx = keepSelection(m);
        renderThread();
      },
      function () {
        enterSelectMode();
        selectIdx = keepSelection(m);
        renderThread();
      });
  }

  // The detail view: who reacted with what, like WhatsApp's reaction sheet.
  function openReactionList(m) {
    var groups = groupReactions(m.reactions);
    if (!groups.length) return;
    elChatMenuTitle.textContent = "Reactions";
    elChatMenuList.innerHTML = "";
    groups.forEach(function (g) {
      g.people.forEach(function (p) {
        var row = document.createElement("div");
        row.className = "menu-item reaction-row";
        row.setAttribute("data-nav", "");
        var who = document.createElement("span");
        who.textContent = p.sendername || p.sender || "Unknown";
        var em = document.createElement("span");
        em.className = "reaction-row-emoji";
        em.textContent = g.emoji;
        row.appendChild(who);
        row.appendChild(em);
        elChatMenuList.appendChild(row);
      });
    });
    elChatMenu.hidden = false;
    var back = function () {
      elChatMenu.hidden = true;
      enterSelectMode();
      setSelect(keep);
      renderThread();
    };
    var keep = selectIdx;
    Nav.setScreen({
      list: elChatMenu,
      onSoftLeft: back, onBack: back, onEnter: back, onSoftRight: back
    });
    Nav.setSoftkeys("Close", "", "OK");
  }

  // WhatsApp's edit window is about 15 minutes; a little slack avoids offering
  // an edit that will certainly be rejected by the time it arrives.
  var EDIT_WINDOW = 14 * 60;
  function withinEditWindow(m) {
    if (!m.ts) return false;
    return (Math.floor(Date.now() / 1000) - m.ts) < EDIT_WINDOW;
  }

  // forceRerender invalidates the incremental-render cache, so the next render
  // rebuilds instead of appending. Needed whenever an EXISTING bubble changed.
  function forceRerender() { renderedIds = []; renderedFor = null; }

  function removeLocalMessage(chat, msgid) {
    var arr = threads[chat] || [];
    for (var i = 0; i < arr.length; i++) {
      if (arr[i].msgid === msgid) { arr.splice(i, 1); break; }
    }
    forceRerender();
    if (currentJID === chat) renderThread();
  }

  // shortName trims a display name to something that fits a 240px menu row.
  function shortName(n) {
    if (!n) return "sender";
    return n.length > 14 ? n.slice(0, 13) + "…" : n;
  }

  // openDirectChat resolves the sender of a group message to their DM and
  // opens it. The chat may not exist locally yet — that's fine, it appears as
  // an empty thread and the first message creates it.
  function openDirectChat(m) {
    pendingProfile = { mode: "dm" };
    W.send(W.T.GETPROFILE, { srcmsgid: m.msgid });
  }

  // ---------- forward picker ----------
  var fwdSource = null; // the message being forwarded
  function openForwardPicker(m) {
    fwdSource = m;
    var picker = document.getElementById("fwd-picker");
    var list = document.getElementById("fwd-list");
    list.innerHTML = "";
    // list all known chats except the current one, newest first
    var arr = Object.keys(chats).map(function (j) { return chats[j]; })
      .filter(function (c) { return c.jid !== currentJID; })
      .sort(function (a, b) { return (b.ts || 0) - (a.ts || 0); });
    arr.forEach(function (c) {
      var row = document.createElement("div");
      row.className = "menu-item";
      row.setAttribute("data-nav", "");
      row.setAttribute("data-jid", c.jid);
      row.textContent = c.name || c.jid;
      row.onclick = function () { doForward(c.jid); };
      list.appendChild(row);
    });
    picker.hidden = false;
    Nav.setScreen({
      list: picker,
      onEnter: function (e, el) { if (el) doForward(el.getAttribute("data-jid")); },
      onSoftLeft: closeForwardPicker,
      onSoftRight: function () {
        var el = Nav.focusedEl();
        if (el) doForward(el.getAttribute("data-jid"));
      },
      onBack: closeForwardPicker
    });
    Nav.setSoftkeys("Cancel", "", "Send");
  }
  function doForward(destJID) {
    if (fwdSource && destJID) {
      W.send(W.T.FORWARD, { srcmsgid: fwdSource.msgid, dest: destJID });
    }
    closeForwardPicker();
  }
  function closeForwardPicker() {
    document.getElementById("fwd-picker").hidden = true;
    fwdSource = null;
    enterComposeMode();
  }

  // ---------- reply state ----------
  function startReply(m) {
    replyingTo = {
      msgid: m.msgid,
      name: m.fromme ? "You" : (m.sendername || (chats[currentJID] && chats[currentJID].name) || ""),
      text: m.kind === "text" ? (m.text || "") : "[" + m.kind + "]"
    };
    elReplyBarText.textContent = "↩ " + replyingTo.name + ": " +
      truncate(replyingTo.text, 40);
    elReplyBar.hidden = false;
  }

  function clearReply() {
    replyingTo = null;
    privateReply = false;
    elReplyBar.hidden = true;
    elReplyBarText.textContent = "";
  }

  function truncate(s, n) {
    return s.length > n ? s.slice(0, n - 1) + "…" : s;
  }

  // Stable per-sender color: hash the sender name to one of a small palette so
  // the same person is always the same color within a group.
  var PALETTE = [
    "#53bdeb", "#e542a3", "#5ed97f", "#f0a441", "#a06cf0",
    "#e5697d", "#4fd1c5", "#d9c34f", "#7d8cf0", "#e57e53"
  ];
  function colorFor(name) {
    if (!name) return PALETTE[0];
    var h = 0;
    for (var i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) & 0xffffffff;
    return PALETTE[Math.abs(h) % PALETTE.length];
  }

  // Derive the daemon's HTTP base from the configured WS URL so /media/<id>
  // can be fetched. ws://host:port -> http://host:port
  function mediaBase() {
    var u = window.CONFIG.DAEMON_WS;
    return u.replace(/^ws/, "http").replace(/\/ws.*$/, "");
  }

  // A bubble shows the THUMBNAIL, never the full-size file.
  //
  // CSS max-width changes how an image is drawn, not how it is decoded: a
  // 1600x1200 photo in a 160px bubble still costs about 7.7 MB of decoded
  // bitmap. A thread with a handful of photos was therefore the largest
  // backgrounded process on the phone, and KaiOS kills the largest first —
  // which is why this app died in seconds where a page with no images ran for
  // hours. WhatsApp ships a small preview inside every media message, so the
  // daemon stores it and serves it at /thumb/, and the full file is only
  // fetched when a photo is opened full-screen.
  function thumbOrFull(m) {
    return mediaBase() + (m.thumb || m.media || "");
  }

  // Messages stored before the daemon kept thumbnails have no row to serve, so
  // a miss falls back to the full file rather than showing a broken image.
  function withThumbFallback(img, m) {
    if (!m.thumb || !m.media) return;
    img.onerror = function () {
      img.onerror = null;
      img.src = mediaBase() + m.media;
    };
  }

  function renderMediaBubble(b, m) {
    var url = mediaBase() + (m.media || "");
    if (m.kind === "image") {
      var img = document.createElement("img");
      img.className = "media-img";
      img.src = thumbOrFull(m);
      withThumbFallback(img, m);
      img.alt = m.text || "photo";
      b.appendChild(img);
      if (m.text) {
        var cap = document.createElement("div");
        cap.className = "caption";
        cap.textContent = m.text;
        b.appendChild(cap);
      }
    } else if (m.kind === "audio") {
      // Voice note. Opus/OGG — plays on desktop; on-device KaiOS support is
      // the open question, but the <audio> element is the right vehicle.
      var au = document.createElement("audio");
      au.className = "media-audio";
      au.controls = true;
      au.preload = "none";
      au.src = url;
      b.appendChild(au);
    } else if (m.kind === "gif") {
      // GIF = muted, looping, autoplaying video (that's how WhatsApp stores it).
      var g = document.createElement("video");
      g.className = "media-img";
      if (m.thumb) g.poster = mediaBase() + m.thumb;
      g.src = url;
      g.autoplay = true;
      g.loop = true;
      g.muted = true;
      g.setAttribute("muted", "");     // some engines need the attribute too
      g.setAttribute("playsinline", "");
      b.appendChild(g);
      // nudge autoplay in case the attribute alone doesn't trigger it
      // play() returns undefined on Gecko 48 — promises came later — so
      // .catch() on it throws and takes the whole render down with it.
      try {
        var p = g.play && g.play();
        if (p && p.catch) p.catch(function () {});
      } catch (e) { /* autoplay refused; the poster frame is fine */ }
    } else if (m.kind === "video") {
      var vid = document.createElement("video");
      vid.className = "media-img";
      vid.src = url;
      vid.controls = true;
      // preload="none" plus a poster: the element shows the small preview and
      // decodes nothing of the video until it is played. With "metadata" the
      // engine pulls and decodes a full-size frame for every video in the
      // thread, which is the same memory problem as the photos.
      vid.preload = "none";
      if (m.thumb) vid.poster = mediaBase() + m.thumb;
      b.appendChild(vid);
      if (m.text) {
        var vcap = document.createElement("div");
        vcap.className = "caption";
        vcap.textContent = m.text;
        b.appendChild(vcap);
      }
    } else if (m.kind === "sticker") {
      // WebP, possibly animated. Renders as an image; animated frames depend
      // on the browser (desktop animates; Gecko 48 likely shows frame 1).
      var stk = document.createElement("img");
      stk.className = "media-sticker";
      stk.src = thumbOrFull(m);
      withThumbFallback(stk, m);
      stk.alt = "sticker";
      b.appendChild(stk);
    } else if (m.kind === "location") {
      renderLocationBubble(b, m);
    } else if (m.kind === "unsupported") {
      var un = document.createElement("div");
      un.className = "media-file unsupported";
      un.textContent = m.text || "[unsupported message]";
      b.appendChild(un);
    } else if (m.kind === "doc") {
      var doc = document.createElement("div");
      doc.className = "media-file";
      doc.textContent = "📄 " + (m.text || "Document");
      b.appendChild(doc);
    } else {
      b.textContent = "[" + m.kind + "]";
    }
  }

  // A location renders as WhatsApp's own map preview (shipped inside the
  // message, so no tiles are fetched and no API key is needed) plus whatever
  // name/address came with it. Selecting it offers to open a real map app.
  function renderLocationBubble(b, m) {
    var card = document.createElement("div");
    card.className = "loc-card";

    if (m.media) {
      var img = document.createElement("img");
      img.className = "loc-map";
      img.src = mediaBase() + m.media;
      img.alt = "map";
      card.appendChild(img);
    } else {
      var ph = document.createElement("div");
      ph.className = "loc-map placeholder";
      ph.textContent = "📍";
      card.appendChild(ph);
    }

    var title = document.createElement("div");
    title.className = "loc-title";
    title.textContent = m.locname || "Location";
    card.appendChild(title);

    if (m.locaddress) {
      var addr = document.createElement("div");
      addr.className = "loc-addr";
      addr.textContent = m.locaddress;
      card.appendChild(addr);
    }
    if (m.lat || m.lon) {
      var co = document.createElement("div");
      co.className = "loc-addr dim";
      co.textContent = m.lat.toFixed(5) + ", " + m.lon.toFixed(5);
      card.appendChild(co);
    }
    b.appendChild(card);
    if (m.text) {
      var cap = document.createElement("div");
      cap.className = "caption";
      cap.textContent = m.text;
      b.appendChild(cap);
    }
  }

  // openLocation hands the coordinates to whatever map app the phone has.
  //
  // A geo: URI is the standard Android/KaiOS way and lets the OS show its own
  // "open with" chooser, which is what we want rather than hardcoding a
  // provider. Desktop browsers don't handle geo:, so there we fall back to
  // OpenStreetMap in a new tab.
  function openLocation(m) {
    if (!m || (!m.lat && !m.lon)) { toast("No coordinates on this message"); return; }
    var label = m.locname || m.locaddress || "Location";
    var geo = "geo:" + m.lat + "," + m.lon + "?q=" +
      m.lat + "," + m.lon + "(" + encodeURIComponent(label) + ")";
    var osm = "https://www.openstreetmap.org/?mlat=" + m.lat +
      "&mlon=" + m.lon + "#map=16/" + m.lat + "/" + m.lon;

    if (window.MozActivity) {
      try {
        var act = new window.MozActivity({ name: "view", data: { type: "url", url: geo } });
        // No map app registered for geo: — fall back to the web map.
        act.onerror = function () { openURL(osm); };
        return;
      } catch (e) {
        // fall through
      }
    }
    openURL(osm);
  }

  function openURL(url) {
    if (window.MozActivity) {
      try {
        new window.MozActivity({ name: "view", data: { type: "url", url: url } });
        return;
      } catch (e) { /* fall through */ }
    }
    window.open(url, "_blank");
  }

  // What the DOM currently shows, so an update can tell "three new messages at
  // the end" from "everything changed".
  var renderedFor = null;
  var renderedIds = [];

  // canAppendOnly is true when the rendered list is a prefix of the current one
  // and nothing about the existing entries changed — i.e. messages were only
  // added at the end. Selection changes, edits, deletions and older pages all
  // fail this and take the full rebuild.
  function canAppendOnly(msgs) {
    // Select mode used to force a full rebuild here, because moving the
    // highlight went through renderThread(). paintSelection() edits the two
    // class lists directly now, so a message arriving while you're reading
    // history only needs its own bubble appended.
    if (renderedFor !== currentJID) return false;
    if (!renderedIds.length) return false;
    if (msgs.length <= renderedIds.length) return false;
    for (var i = 0; i < renderedIds.length; i++) {
      if (msgs[i].msgid !== renderedIds[i]) return false;
    }
    return true;
  }

  // appendNewBubbles draws only the tail. Deliberately reuses buildBubble so
  // there is one definition of what a message looks like.
  function appendNewBubbles(msgs, isGroup, wasNearBottom) {
    var lastTS = 0, lastSender = null;
    if (renderedIds.length) {
      var prev = msgs[renderedIds.length - 1];
      lastTS = prev.ts || 0;
      lastSender = prev.fromme ? null : prev.sendername;
    }
    for (var i = renderedIds.length; i < msgs.length; i++) {
      lastSender = buildBubble(msgs[i], i, isGroup, lastTS, lastSender);
      lastTS = msgs[i].ts || lastTS;
      renderedIds.push(msgs[i].msgid);
    }
    // Don't chase the bottom while a bubble is selected — that would yank the
    // reader away from the message they're standing on.
    if (wasNearBottom && !selectMode) elThreadMsgs.scrollTop = elThreadMsgs.scrollHeight;
  }

  // buildBubble draws one message (plus any date separator, sender label and
  // reaction row it needs) and returns the sender to carry into the next one.
  // Shared by the full rebuild and the append-only path so a bubble is defined
  // in exactly one place.
  function buildBubble(m, idx, isGroup, lastTS, lastSender) {
      // Date separator whenever the day changes (and before the first message).
    if (m.ts && !sameDay(lastTS, m.ts)) {
      var sep = document.createElement("div");
      sep.className = "day-sep";
      sep.textContent = dayLabel(m.ts);
      elThreadMsgs.appendChild(sep);
      lastSender = null; // re-label the sender after a break in the day
    }
    // In groups, label each incoming message with its sender — but only when
    // the sender changes, so runs from one person aren't repetitive.
    if (isGroup && !m.fromme && m.sendername && m.sendername !== lastSender) {
      var who = document.createElement("div");
      who.className = "sender";
      who.textContent = m.sendername;
      who.style.color = colorFor(m.sendername);
      elThreadMsgs.appendChild(who);
    }
    var b = document.createElement("div");
    b.className = "bubble " + (m.fromme ? "me" : "them");
    // mark the selected bubble in select mode
    if (selectMode && idx === selectIdx) b.className += " selected";
    // tint incoming group bubbles' left border with the sender color
    if (isGroup && !m.fromme && m.sendername) {
      b.style.borderLeft = "2px solid " + colorFor(m.sendername);
    }
    // WhatsApp's forwarded marker, above any quote bar — same order the
    // official clients use.
    if (m.forwarded) {
      var fwd = document.createElement("div");
      fwd.className = "fwd-label";
      fwd.textContent = "↪ Forwarded";
      b.appendChild(fwd);
    }
    // if this message quotes another, show a small quote bar first
    if (m.quotedtext) {
      var q = document.createElement("div");
      q.className = "quote";
      var qn = document.createElement("div");
      qn.className = "quote-name";
      qn.textContent = m.quotedname || "";
      var qt = document.createElement("div");
      qt.className = "quote-text";
      qt.textContent = truncate(m.quotedtext, 60);
      q.appendChild(qn);
      q.appendChild(qt);
      b.appendChild(q);
    }
    if (m.deleted) {
      b.className += " deleted-msg";
      var del = document.createElement("span");
      del.textContent = "🚫 deleted";
      b.appendChild(del);
    } else if (m.kind === "text") {
      var body = document.createElement("span");
      body.textContent = m.text;
      b.appendChild(body);
    } else {
      renderMediaBubble(b, m);
    }

    // Time, and for our own messages the delivery ticks, on one trailing line.
    var meta = document.createElement("div");
    meta.className = "meta";
    var t = document.createElement("span");
    t.textContent = (m.edited ? "edited · " : "") + timeOf(m.ts);
    meta.appendChild(t);
    if (m.fromme && !m.deleted) {
      var tick = tickFor(m.status);
      var tk = document.createElement("span");
      tk.className = tick.cls;
      tk.textContent = tick.mark;
      meta.appendChild(tk);
    }
    b.appendChild(meta);
    b.setAttribute("data-msgid", m.msgid);
    elThreadMsgs.appendChild(b);
    // Reactions hang below the bubble, grouped by emoji with a count.
    var groups = groupReactions(m.reactions);
    if (groups.length) {
      var row = document.createElement("div");
      row.className = "reactions " + (m.fromme ? "me" : "them");
      groups.forEach(function (g) {
        var chip = document.createElement("span");
        chip.className = "reaction-chip";
        chip.textContent = g.emoji + (g.people.length > 1 ? " " + g.people.length : "");
        row.appendChild(chip);
      });
      elThreadMsgs.appendChild(row);
    }
    return m.fromme ? null : m.sendername;
  }

  function renderThread() {
    var msgs = threads[currentJID] || [];
    var isGroup = chats[currentJID] && chats[currentJID].group;
    // Was the user near the bottom BEFORE we rebuild? If so we'll auto-scroll
    // after; if they'd scrolled up to read history, we leave them there.
    var wasNearBottom = forceScrollBottom || (elThreadMsgs.scrollHeight - elThreadMsgs.scrollTop
      - elThreadMsgs.clientHeight) < 60;
    forceScrollBottom = false;
    // Rebuilding every bubble on every message is O(thread) per message, which
    // is fine in Chrome and ruinous on Gecko 48 once a thread is long. When the
    // only change is messages appended at the end — the common case by far — we
    // append those instead and leave the rest of the DOM alone.
    if (canAppendOnly(msgs)) {
      appendNewBubbles(msgs, isGroup, wasNearBottom);
      return;
    }
    renderedFor = currentJID;
    renderedIds = [];
    elThreadMsgs.innerHTML = "";
    var lastSender = null;
    var lastTS = 0;
    msgs.forEach(function (m, idx) {
      lastSender = buildBubble(m, idx, isGroup, lastTS, lastSender);
      if (m.ts) lastTS = m.ts;
      renderedIds.push(m.msgid);
    });
    // in select mode, scroll the selected bubble into view; else stick to bottom
    if (selectMode) {
      var sel = elThreadMsgs.querySelector(".bubble.selected");
      if (sel) Nav.ensureVisible(sel, elThreadMsgs);
    } else if (wasNearBottom) {
      elThreadMsgs.scrollTop = elThreadMsgs.scrollHeight;
    }
  }

  function sendCurrent() {
    var text = elInput.value.trim();
    if (!text || !currentJID) return;
    var frame = { chat: currentJID, kind: "text", text: text };
    if (replyingTo) frame.quoted = replyingTo.msgid;
    if (privateReply) frame.private = true;
    W.send(W.T.SEND, frame);
    // A private reply leaves this chat entirely — the daemon routes it to the
    // sender's DM — so don't echo it into the group thread. The receipt frame
    // carries the destination and the message lands there instead.
    if (!privateReply) {
      var echo = {
        msgid: "local-" + Date.now(), chat: currentJID, fromme: true,
        ts: Math.floor(Date.now() / 1000), kind: "text", text: text,
        status: "sent"
      };
      if (replyingTo) {
        echo.quotedtext = replyingTo.text;
        echo.quotedname = replyingTo.name;
      }
      pushMsg(echo);
    } else {
      toast("Sent privately");
    }
    elInput.value = "";
    clearDraft(currentJID);
    stopTyping();
    clearReply();
  }

  // ---------- contact / group info screen ----------
  function openProfile(jid, backFn) {
    pendingProfile = { mode: "show" };
    profileBack = backFn || enterListScreen;
    W.send(W.T.GETPROFILE, { jid: jid });
  }

  function openProfileForMessage(m, backFn) {
    pendingProfile = { mode: "show" };
    profileBack = backFn || enterListScreen;
    W.send(W.T.GETPROFILE, { srcmsgid: m.msgid });
  }

  function renderProfile(p) {
    profileData = p;
    elProfileBody.innerHTML = "";

    var av = document.createElement("div");
    av.className = "profile-avatar";
    av.textContent = initialsFor(p.name || p.jid);
    av.style.background = colorFor(p.name || p.jid);
    if (p.avatar) {
      var img = new Image();
      img.className = "avatar-img";
      img.onload = function () {
        av.textContent = ""; av.style.background = "none"; av.appendChild(img);
      };
      img.src = mediaBase() + p.avatar;
    }
    elProfileBody.appendChild(av);

    var nm = document.createElement("div");
    nm.className = "profile-name";
    nm.textContent = p.name || p.jid;
    elProfileBody.appendChild(nm);

    // The number, or the best we can do. For someone never saved and only
    // known by LID, WhatsApp may expose just a redacted form — say so plainly
    // instead of showing a meaningless internal id.
    var sub = document.createElement("div");
    sub.className = "profile-sub";
    if (p.phone) sub.textContent = p.phone;
    else if (p.redactedphone) sub.textContent = p.redactedphone + " (hidden)";
    else if (!p.group) sub.textContent = "Number not available";
    else sub.textContent = p.members ? p.members + " participants" : "Group";
    elProfileBody.appendChild(sub);

    // If we're showing a name the user chose, note what the contact calls
    // themselves underneath. Compare with the tilde stripped: for an unsaved
    // person the heading IS their own name, already marked, and repeating it
    // here would just print the same string twice.
    if (!p.group && p.pushname && p.pushname !== (p.name || "").replace(/^~/, "")) {
      var pn = document.createElement("div");
      pn.className = "profile-sub dim";
      pn.textContent = "~" + p.pushname + (p.business ? " (business)" : "");
      elProfileBody.appendChild(pn);
    }
    if (p.status) {
      var st = document.createElement("div");
      st.className = "profile-status";
      st.textContent = p.status;
      elProfileBody.appendChild(st);
    }

    var list = document.createElement("div");
    list.className = "profile-actions";
    profileActions(p).forEach(function (it) {
      var row = document.createElement("div");
      row.className = "menu-item" + (it.danger ? " danger" : "");
      row.setAttribute("data-nav", "");
      row.setAttribute("data-action", it.action);
      row.textContent = it.label;
      list.appendChild(row);
    });
    elProfileBody.appendChild(list);

    if (p.group && p.memberlist && p.memberlist.length) {
      var mh = document.createElement("div");
      mh.className = "profile-sub dim";
      mh.textContent = "Participants";
      elProfileBody.appendChild(mh);
      p.memberlist.forEach(function (mem) {
        var r = document.createElement("div");
        r.className = "member-row";
        r.textContent = mem.name + (mem.admin ? " • admin" : "");
        elProfileBody.appendChild(r);
      });
      if (p.members > p.memberlist.length) {
        var more = document.createElement("div");
        more.className = "member-row dim";
        more.textContent = "+" + (p.members - p.memberlist.length) + " more";
        elProfileBody.appendChild(more);
      }
    }

    show(elProfile);
    Nav.setScreen({
      list: elProfile,
      onEnter: function (e, el) { if (el) runProfileAction(el.getAttribute("data-action")); },
      onSoftRight: function () {
        var el = Nav.focusedEl();
        if (el) runProfileAction(el.getAttribute("data-action"));
      },
      onSoftLeft: function () { (profileBack || enterListScreen)(); },
      onBack: function () { (profileBack || enterListScreen)(); }
    });
    // Back is the red key; the left softkey stays free.
    Nav.setSoftkeys("", "OK", "OK");
  }

  function profileActions(p) {
    var items = [{ action: "message", label: "Message" }];
    if (!p.group) {
      items.push({
        action: "savename",
        label: p.savedname ? "Edit saved name" : "Save as contact"
      });
      // Writing to the phone's own address book is a separate thing from the
      // in-app nickname, and only possible on the device.
      if (p.phone && deviceContactsAvailable()) {
        items.push({ action: "addtophone", label: "Add to phone contacts" });
      }
      if (p.savedname) items.push({ action: "clearname", label: "Remove saved name" });
    }
    items.push({ action: "pin", label: p.pinned ? "Unpin chat" : "Pin chat" });
    items.push({ action: "mute", label: p.muted ? "Unmute" : "Mute" });
    items.push({ action: "archive", label: p.archived ? "Unarchive" : "Archive" });
    return items;
  }

  function runProfileAction(action) {
    var p = profileData;
    if (!p) return;
    if (action === "message") {
      openThread(p.jid);
    } else if (action === "savename") {
      textPrompt("Save contact name", p.savedname || p.name || "",
        "Saved in this app. Use “Add to phone contacts” for the address book.",
        function (val) {
          W.send(W.T.SAVECONTACT, { jid: p.jid, name: val });
          pendingProfile = { mode: "show" };
        },
        function () { renderProfile(p); });
    } else if (action === "clearname") {
      W.send(W.T.SAVECONTACT, { jid: p.jid, name: "" });
      pendingProfile = { mode: "show" };
    } else if (action === "addtophone") {
      addToDeviceContacts(p);
    } else if (action === "pin" || action === "mute" || action === "archive") {
      var on = !p[action === "pin" ? "pinned" : action === "mute" ? "muted" : "archived"];
      W.send(W.T.CHATACTION, { chat: p.jid, action: action, on: on });
      if (action === "pin") p.pinned = on;
      else if (action === "mute") p.muted = on;
      else p.archived = on;
      renderProfile(p);
    }
  }

  // ---------- device address book (KaiOS only) ----------
  // Two routes, in order of preference:
  //   1. navigator.mozContacts — writes directly, needs the "contacts"
  //      permission granted to a privileged app.
  //   2. a "new/webcontacts/contact" web activity — hands the details to the
  //      phone's Contacts app with fields prefilled, which is the native UX
  //      and needs no extra permission.
  // Neither exists in a desktop browser, so the action is hidden there and the
  // in-app nickname is the only thing that happens.
  function deviceContactsAvailable() {
    return !!(navigator.mozContacts || window.MozActivity);
  }

  function addToDeviceContacts(p) {
    var name = p.savedname || p.name || p.phone;
    var tel = p.phone;
    if (!tel) { toast("No phone number to save"); return; }

    if (navigator.mozContacts && window.mozContact) {
      try {
        var c = new window.mozContact();
        c.init({ name: [name], givenName: [name], tel: [{ type: ["mobile"], value: tel }] });
        var req = navigator.mozContacts.save(c);
        req.onsuccess = function () { toast("Saved to phone contacts"); };
        req.onerror = function () { activityAddContact(name, tel); };
        return;
      } catch (e) {
        // fall through to the activity route
      }
    }
    activityAddContact(name, tel);
  }

  function activityAddContact(name, tel) {
    if (!window.MozActivity) { toast("Contacts not available here"); return; }
    try {
      var act = new window.MozActivity({
        name: "new",
        data: { type: "webcontacts/contact", params: { givenName: name, tel: tel } }
      });
      act.onerror = function () { toast("Could not open Contacts"); };
    } catch (e) {
      toast("Could not open Contacts");
    }
  }

  // ---------- prompt overlays ----------
  // One-line text input and a yes/no confirm, both driven by softkeys since
  // there's no pointer. Each restores the caller's screen via its cancel fn.
  function textPrompt(title, initial, hint, onOK, onCancel) {
    elPromptTitle.textContent = title;
    elPromptHint.textContent = hint || "";
    elPromptInput.value = initial || "";
    elPrompt.hidden = false;
    Nav.setScreen({
      onSoftLeft: function () { elPrompt.hidden = true; if (onCancel) onCancel(); },
      onBack: function () { elPrompt.hidden = true; if (onCancel) onCancel(); },
      onSoftRight: done,
      onEnter: done
    });
    Nav.setSoftkeys("Cancel", "", "Save");
    setTimeout(function () { elPromptInput.focus(); }, 0);
    function done() {
      var v = elPromptInput.value.trim();
      elPrompt.hidden = true;
      if (onOK) onOK(v);
    }
  }

  function confirmPrompt(title, hint, onYes, onNo) {
    elPromptTitle.textContent = title;
    elPromptHint.textContent = hint || "";
    elPromptInput.value = "";
    elPromptInput.hidden = true;
    elPrompt.hidden = false;
    Nav.setScreen({
      onSoftLeft: no, onBack: no,
      onSoftRight: yes, onEnter: yes
    });
    Nav.setSoftkeys("No", "", "Yes");
    function cleanup() { elPrompt.hidden = true; elPromptInput.hidden = false; }
    function yes() { cleanup(); if (onYes) onYes(); }
    function no() { cleanup(); if (onNo) onNo(); }
  }

  // chooseFromList: pick one of a few labels. Reuses the chat-menu overlay the
  // way the attach menu does, rather than adding another screen. onPick gets
  // the index, or -1 if the user backed out.
  function chooseFromList(title, labels, onPick) {
    elChatMenuTitle.textContent = title;
    elChatMenuList.innerHTML = "";
    labels.forEach(function (label, i) {
      var row = document.createElement("div");
      row.className = "menu-item";
      row.setAttribute("data-nav", "");
      row.setAttribute("data-idx", String(i));
      row.textContent = label;
      elChatMenuList.appendChild(row);
    });
    elChatMenu.hidden = false;
    function done(idx) {
      elChatMenu.hidden = true;
      enterComposeMode();
      if (onPick) onPick(idx);
    }
    function take(el) { done(el ? parseInt(el.getAttribute("data-idx"), 10) : -1); }
    Nav.setScreen({
      list: elChatMenu,
      onEnter: function (e, el) { take(el); },
      onSoftRight: function () { take(Nav.focusedEl()); },
      onSoftLeft: function () { done(-1); },
      onBack: function () { done(-1); }
    });
    Nav.setSoftkeys("Cancel", "", "OK");
  }

  // toast: a brief status line in the header. No notification API assumptions.
  var toastTimer = null;
  var toastPrev = null;
  function toast(msg) {
    var hdr = document.querySelector("header span");
    if (!hdr) return;
    // Capture the real title once — a second toast within the window would
    // otherwise "restore" the first toast's text permanently.
    if (toastPrev === null) toastPrev = hdr.textContent;
    hdr.textContent = msg;
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function () {
      hdr.textContent = toastPrev;
      toastPrev = null;
    }, 2500);
  }

  function initialsFor(s) {
    return (s || "?").replace(/[^A-Za-z0-9 ]/g, "").trim().split(/\s+/).slice(0, 2)
      .map(function (w) { return w.charAt(0).toUpperCase(); }).join("") || "?";
  }

  // ---------- full-screen media viewer ----------
  // Tapping a photo should show it, not download it. Left/Right steps through
  // the other images in the same chat so browsing doesn't mean backing out to
  // the thread between every one.
  var viewerList = [];
  var viewerIdx = 0;

  function openViewer(m) {
    var msgs = threads[currentJID] || [];
    viewerList = msgs.filter(function (x) {
      return (x.kind === "image" || x.kind === "sticker") && x.media && !x.deleted;
    });
    viewerIdx = 0;
    for (var i = 0; i < viewerList.length; i++) {
      if (viewerList[i].msgid === m.msgid) { viewerIdx = i; break; }
    }
    if (!viewerList.length) { toast("Nothing to show"); return; }
    showViewerAt(viewerIdx);

    elViewer.hidden = false;
    Nav.setScreen({
      onLeft: function () { stepViewer(-1); },
      onRight: function () { stepViewer(1); },
      onUp: function () { stepViewer(-1); },
      onDown: function () { stepViewer(1); },
      onSoftLeft: closeViewer,
      onBack: closeViewer,
      onSoftRight: function () { saveCurrentImage(); },
      onEnter: function () { saveCurrentImage(); }
    });
    Nav.setSoftkeys("Close", "", "Save");
  }

  function showViewerAt(i) {
    var m = viewerList[i];
    if (!m) return;
    elViewerImg.src = mediaBase() + m.media;
    var caption = m.text || "";
    var pos = viewerList.length > 1 ? (i + 1) + "/" + viewerList.length : "";
    elViewerCap.textContent = [pos, caption].filter(Boolean).join("  ");
  }

  function stepViewer(delta) {
    var next = viewerIdx + delta;
    if (next < 0 || next >= viewerList.length) return;
    viewerIdx = next;
    showViewerAt(viewerIdx);
  }

  function closeViewer() {
    elViewer.hidden = true;
    elViewerImg.src = "";
    enterSelectMode();
    renderThread();
  }

  // Saving hands the image to the phone rather than triggering a browser
  // download — on KaiOS a "download" isn't a meaningful concept.
  function saveCurrentImage() {
    var m = viewerList[viewerIdx];
    if (!m) return;
    var url = mediaBase() + m.media;
    if (window.MozActivity) {
      try {
        new window.MozActivity({ name: "view", data: { type: "url", url: url } });
        return;
      } catch (e) { /* fall through */ }
    }
    window.open(url, "_blank");
  }

  // ---------- sending a location ----------
  //
  // geolocation is granted to a plain web-type app, so unlike recording audio
  // this needs no privilege bump — it only needs asking. The phone still
  // prompts the user the first time.
  //
  // Two shapes: a one-shot pin, and a live share that keeps sending until it
  // expires or is stopped. The daemon owns the live session (it has to — it
  // holds the WhatsApp connection and the sequence numbering); the app's job is
  // to produce fixes and forward them.

  function haveGeolocation() {
    return !!(navigator.geolocation && navigator.geolocation.getCurrentPosition);
  }

  // A first fix from cold on this hardware can take a while, and the phone
  // gives no feedback while it tries — so say something, or it reads as a
  // dead menu item.
  var GEO_TIMEOUT_MS = 30000;

  function getFix(onFix, onFail) {
    if (!haveGeolocation()) { onFail("This phone has no location support"); return; }
    var done = false;
    var t = setTimeout(function () {
      if (!done) { done = true; onFail("Couldn't get a location fix"); }
    }, GEO_TIMEOUT_MS + 2000);
    try {
      navigator.geolocation.getCurrentPosition(
        function (pos) {
          if (done) return;
          done = true; clearTimeout(t);
          onFix(pos);
        },
        function (err) {
          if (done) return;
          done = true; clearTimeout(t);
          // PERMISSION_DENIED is 1 and is the one worth naming, because the
          // fix is a settings change and not "try again".
          onFail(err && err.code === 1
            ? "Location permission refused"
            : "Couldn't get a location fix");
        },
        { enableHighAccuracy: true, timeout: GEO_TIMEOUT_MS, maximumAge: 60000 }
      );
    } catch (e) {
      done = true; clearTimeout(t);
      onFail("Couldn't get a location fix");
    }
  }

  function sendLocation() {
    if (!currentJID) return;
    var chat = currentJID;
    toast("Getting location…");
    getFix(function (pos) {
      var c = pos.coords || {};
      W.send(W.T.SEND, {
        chat: chat, kind: "location",
        lat: c.latitude, lon: c.longitude,
        acc: Math.round(c.accuracy || 0),
        quoted: replyingTo ? replyingTo.msgid : ""
      });
      clearReply();
      toast("Location sent");
    }, function (msg) { toast(msg); });
  }

  // ---------- live location ----------
  //
  // Sharing continuously off a 1400 mAh battery is the expensive part, not the
  // protocol. watchPosition with high accuracy holds the GPS on, so updates go
  // out on a timer rather than on every fix the chip produces, and the share
  // stops itself at the chosen duration even if the app is never reopened.
  var LIVE_DURATIONS = [
    { label: "15 minutes", secs: 15 * 60 },
    { label: "1 hour", secs: 60 * 60 },
    { label: "8 hours", secs: 8 * 60 * 60 }
  ];
  var LIVE_UPDATE_MS = 30000;   // one update every 30s, not every fix

  var liveShare = {
    active: false, chat: "", watchId: null, timer: null,
    endsAt: 0, last: null, lastSentAt: 0
  };

  function startLiveLocation() {
    if (!currentJID) return;
    if (liveShare.active) { stopLiveLocation("Stopped sharing"); return; }
    var chat = currentJID;
    chooseFromList("Share live location for…",
      LIVE_DURATIONS.map(function (d) { return d.label; }),
      function (idx) {
        if (idx < 0) return;
        beginLiveShare(chat, LIVE_DURATIONS[idx].secs);
      });
  }

  function beginLiveShare(chat, secs) {
    toast("Getting location…");
    getFix(function (pos) {
      var c = pos.coords || {};
      liveShare.active = true;
      liveShare.chat = chat;
      liveShare.endsAt = Date.now() + secs * 1000;
      liveShare.last = c;
      liveShare.lastSentAt = Date.now();

      W.send(W.T.LIVELOC, {
        chat: chat, action: "start", secs: secs,
        lat: c.latitude, lon: c.longitude, acc: Math.round(c.accuracy || 0)
      });
      toast("Sharing live location");
      paintLiveBar();

      // Keep the last fix fresh, but only transmit on the timer.
      try {
        liveShare.watchId = navigator.geolocation.watchPosition(
          function (p) { liveShare.last = p.coords || liveShare.last; },
          function () { /* a dropped fix is normal indoors; keep the last one */ },
          { enableHighAccuracy: true, maximumAge: 15000 }
        );
      } catch (e) { /* the timer still sends the fixes we do get */ }

      liveShare.timer = setInterval(pumpLiveShare, LIVE_UPDATE_MS);
    }, function (msg) { toast(msg); });
  }

  function pumpLiveShare() {
    if (!liveShare.active) return;
    if (Date.now() >= liveShare.endsAt) { stopLiveLocation("Live location ended"); return; }
    var c = liveShare.last;
    if (!c) return;
    W.send(W.T.LIVELOC, {
      chat: liveShare.chat, action: "update",
      lat: c.latitude, lon: c.longitude, acc: Math.round(c.accuracy || 0)
    });
    liveShare.lastSentAt = Date.now();
    paintLiveBar();
  }

  function stopLiveLocation(msg) {
    if (!liveShare.active) return;
    W.send(W.T.LIVELOC, { chat: liveShare.chat, action: "stop" });
    if (liveShare.watchId !== null && navigator.geolocation &&
        navigator.geolocation.clearWatch) {
      try { navigator.geolocation.clearWatch(liveShare.watchId); } catch (e) {}
    }
    if (liveShare.timer) clearInterval(liveShare.timer);
    liveShare.active = false;
    liveShare.watchId = null;
    liveShare.timer = null;
    liveShare.last = null;
    paintLiveBar();
    if (msg) toast(msg);
  }

  // A share that is running with nothing on screen to say so is a battery leak
  // the user can't see, so the thread carries a bar while it lasts.
  function paintLiveBar() {
    var bar = document.getElementById("live-bar");
    if (!bar) return;
    if (!liveShare.active || liveShare.chat !== currentJID) { bar.hidden = true; return; }
    var left = Math.max(0, liveShare.endsAt - Date.now());
    var mins = Math.ceil(left / 60000);
    bar.textContent = "📡 Sharing live location · " +
      (mins >= 60 ? Math.ceil(mins / 60) + "h left" : mins + "m left");
    bar.hidden = false;
  }

  function runAttach(action) {
    if (action === "photo") return pickAndSend("image");
    if (action === "video") return pickAndSend("video");
    if (action === "audio") return pickAndSend("audio");
    if (action === "location") return sendLocation();
    if (action === "livelocation") return startLiveLocation();
    return pickAndSend("doc");
  }

  // ---------- sending attachments ----------
  // The bytes come from the phone's own Camera/Gallery via a pick activity,
  // which is the only way to reach them — an app can't browse the filesystem.
  // On desktop there's a hidden file input instead, so this is testable.
  // A pick activity only opens if some app on the phone has registered as a
  // handler for that MIME type. "*/*" matches no handler on a stock KaiOS
  // build — there is no Files app to answer it — so asking for it fails with
  // NO_PROVIDER and the picker never appears. Ask for concrete types instead,
  // and keep a list of fallbacks: which app answers depends on the handset, so
  // one refusal is not the same as "this phone can't do it".
  var PICK_TYPES = {
    image: [["image/jpeg", "image/png"], ["image/*"], ["image/jpeg"]],
    video: [["video/*"], ["video/mp4", "video/3gpp"]],
    audio: [["audio/*"], ["audio/mpeg", "audio/amr", "audio/ogg"]],
    // Documents genuinely have no owner on most builds. Try a Files app if one
    // is installed, then the concrete types a mail or reader app may claim,
    // then the media apps — a "file" a phone can actually reach is usually a
    // photo or a video anyway.
    doc: [["*/*"], ["application/pdf", "text/plain"], ["image/*"], ["video/*"], ["audio/*"]]
  };

  function pickAndSend(kind) {
    if (!currentJID) return;
    if (window.MozActivity) {
      tryPick(PICK_TYPES[kind] || PICK_TYPES.doc, 0, kind);
      return;
    }
    desktopFilePick(kind);
  }

  // Walk the candidate types until one opens. Every failure mode here is
  // asynchronous except the constructor throwing, so both paths advance.
  function tryPick(candidates, i, kind) {
    if (i >= candidates.length) {
      toast(kind === "doc"
        ? "No app on this phone can pick a file"
        : "No app on this phone can pick that");
      return;
    }
    var act, opened = Date.now();
    try {
      act = new window.MozActivity({ name: "pick", data: { type: candidates[i] } });
    } catch (e) {
      tryPick(candidates, i + 1, kind);
      return;
    }
    act.onsuccess = function () {
      var blob = this.result && this.result.blob;
      if (!blob) { toast("Nothing picked"); return; }
      // What came back decides how it's sent, not what was asked for: pick a
      // photo from the "File" menu and it should still arrive as a photo.
      var mime = blob.type || "";
      var real = kind;
      if (kind === "doc") {
        if (mime.indexOf("image/") === 0) real = "image";
        else if (mime.indexOf("video/") === 0) real = "video";
        else if (mime.indexOf("audio/") === 0) real = "audio";
      }
      blobToBase64AndSend(blob, real, this.result.name || "");
    };
    act.onerror = function () {
      // NO_PROVIDER means nothing handles this type — try the next one. A
      // cancel must NOT retry, or backing out of the picker would reopen it
      // four more times. Some builds report a bare error with no name, so when
      // the name is missing fall back to timing: a refusal comes back at once,
      // whereas a cancel takes as long as it took the user to press a key.
      var name = (this.error && this.error.name) || "";
      var instant = Date.now() - opened < 400;
      if (name === "NO_PROVIDER" || (name === "" && instant)) {
        tryPick(candidates, i + 1, kind);
      } else if (name && name !== "ActivityCanceled") {
        toast("Picker failed: " + name);
      }
    };
  }

  // Desktop fallback: a throwaway file input, so attachments can be exercised
  // in a browser during development.
  function desktopFilePick(kind) {
    var input = document.createElement("input");
    input.type = "file";
    input.accept = kind === "image" ? "image/*" : "*/*";
    input.style.display = "none";
    document.body.appendChild(input);
    input.onchange = function () {
      var f = input.files && input.files[0];
      if (f) blobToBase64AndSend(f, kind, f.name || "");
      document.body.removeChild(input);
    };
    input.click();
  }

  function blobToBase64AndSend(blob, kind, filename) {
    var reader = new FileReader();
    reader.onload = function () {
      // FileReader gives a data: URL; the daemon strips the prefix, but send
      // the payload only so the frame isn't needlessly larger.
      var out = String(reader.result || "");
      var comma = out.indexOf(",");
      var b64 = comma >= 0 ? out.slice(comma + 1) : out;
      toast("Sending…");
      W.send(W.T.SEND, {
        chat: currentJID, kind: kind, media: b64,
        mime: blob.type || "", filename: filename,
        text: "", quoted: replyingTo ? replyingTo.msgid : ""
      });
      clearReply();
    };
    reader.onerror = function () { toast("Couldn't read that file"); };
    reader.readAsDataURL(blob);
  }

  // ---------- search ----------
  var searchDebounce = null;

  function enterSearchScreen() {
    show(elSearch);
    elSearchResults.innerHTML = "";
    elSearchInput.value = "";
    Nav.setScreen({
      list: elSearch,
      // Arrows move the text caret while the box has focus, so stepping into
      // the results has to take focus off it — otherwise up and down just run
      // along the query you typed.
      onFocusChange: function (el) {
        if (el && el !== elSearchInput && document.activeElement === elSearchInput) {
          try { elSearchInput.blur(); } catch (e) {}
        }
      },
      // No Back softkey: the red key goes back. Softkeys are for actions.
      onBack: enterListScreen,
      onEnter: function (e, el) {
        // Enter on a result opens its chat; Enter in the box just searches.
        if (el && el.getAttribute("data-jid")) {
          openThread(el.getAttribute("data-jid"));
        } else {
          runSearch();
        }
      },
      onSoftRight: runSearch
    });
    Nav.setSoftkeys("", "SEARCH", "Search");
    setTimeout(function () { elSearchInput.focus(); }, 0);
  }

  function runSearch() {
    var q = elSearchInput.value.trim();
    if (q.length < 2) { toast("Type at least 2 characters"); return; }
    W.send(W.T.SEARCH, { q: q, limit: 60 });
  }

  function scheduleSearch() {
    if (searchDebounce) clearTimeout(searchDebounce);
    // Wait for a pause in typing: every keystroke scanning the history would
    // be wasteful, and on a keypad each character takes a moment anyway.
    searchDebounce = setTimeout(runSearch, 500);
  }

  function renderSearchResults(d) {
    elSearchResults.innerHTML = "";
    var results = (d && d.results) || [];
    if (!results.length) {
      var none = document.createElement("div");
      none.className = "empty";
      none.textContent = "No messages found";
      elSearchResults.appendChild(none);
      return;
    }
    results.forEach(function (r) {
      var row = document.createElement("div");
      row.className = "search-row";
      row.setAttribute("data-nav", "");
      row.setAttribute("data-jid", r.chat);
      row.onclick = function () { openThread(r.chat); };

      var head = document.createElement("div");
      head.className = "search-head";
      head.textContent = (r.chatname || r.chat) + " · " +
        (r.fromme ? "You" : (r.sendername || "")) + " · " + timeOf(r.ts);
      var body = document.createElement("div");
      body.className = "search-body";
      body.textContent = truncate(r.text || "[" + r.kind + "]", 70);
      row.appendChild(head);
      row.appendChild(body);
      elSearchResults.appendChild(row);
    });
    Nav.refreshFocus();
  }

  // ---------- incoming call screen ----------
  function showIncomingCall(d) {
    activeCall = d;
    elCallName.textContent = (d.name || d.from) + (d.video ? " (video)" : "");
    show(elCall);
    Nav.setScreen({
      onSoftLeft: rejectCall,   // red
      onSoftRight: answerCall,  // green
      onBack: rejectCall
    });
    Nav.setSoftkeys("Reject", "", "Answer");
  }

  function answerCall() {
    if (!activeCall) return;
    W.send(W.T.CALLANSWER, { callid: activeCall.callid });
    // NOTE: real audio needs the WebRTC leg (later). For now this just tells
    // the daemon we accepted; you'll see the call-state change in logs.
  }

  function rejectCall() {
    if (!activeCall) return;
    W.send(W.T.CALLREJECT, { callid: activeCall.callid });
    activeCall = null;
    enterListScreen();
  }

  // ---------- data handlers ----------
  function pushMsg(m) {
    (threads[m.chat] = threads[m.chat] || []).push(m);
    var c = chats[m.chat] = chats[m.chat] || { jid: m.chat, name: m.chat };
    // Prefer the daemon-resolved chat name (group subject or contact name).
    if (m.chatname) c.name = m.chatname;
    c.group = !!m.group;
    if (typeof m.pinned === "boolean") c.pinned = m.pinned;
    c.ts = m.ts;
    // In a group, prefix the preview with who spoke so the list is legible.
    var body = m.kind === "text" ? m.text : "[" + m.kind + "]";
    c.preview = (m.group && !m.fromme && m.sendername)
      ? m.sendername + ": " + body
      : body;
    if (!m.fromme && currentJID !== m.chat) {
      c.unread = (c.unread || 0) + 1;
    } else if (!m.fromme) {
      // It arrived while the user is looking at the chat, so it's read now.
      W.send(W.T.MARKREAD, { jid: m.chat });
    }

    if (currentJID === m.chat) {
      // Don't disrupt an open action menu; refresh underneath otherwise.
      // In select mode a new message shifts indices, so keep the selection
      // pinned to the same message by bumping the index when appending at end.
      if (menuOpen) {
        // leave the view alone; the new message is in the array and will show
        // when the menu closes and renderThread runs again.
      } else {
        renderThread();
      }
    } else if (!elList.hidden) renderChatList();

    alertForMessage(m, c);
  }

  // ---------- in-app notifications ----------
  //
  // A push notification is for when the app ISN'T open. When it is, the OS
  // bubble is the wrong tool — it covers the screen you're using to say
  // something the app could show you in a strip. So:
  //
  //   on the chat list  — buzz only. The list is already the notification: the
  //                       row jumps to the top with a green unread badge.
  //   anywhere else     — a thin bar at the top, plus the buzz, because you
  //                       can't see the list from inside a chat.
  //   in the chat it    — see VIBRATE_IN_OPEN_CHAT. The message appears in
  //   came from           front of you, so buzzing is arguably just noise.
  //
  // The bar holds for NOTIF_HOLD_MS or until another chat interrupts it,
  // whichever comes first. The notification itself outlives the bar: the left
  // softkey stays bound to it so a strip you missed is still reachable.
  var NOTIF_HOLD_MS = 4000;
  var pendingNotif = null;   // {jid, title, body} — survives the bar hiding
  var notifTimer = null;

  // ---- alerting: beep, buzz, or both, following the phone's own profile ----
  //
  // A phone already knows whether its owner wants noise: that's what the ringer
  // profile is for. Rather than inventing a second setting the user has to keep
  // in sync, ask the device via mozSettings and follow it. Two keys matter —
  // audio.volume.notification (0 means silenced) and vibration.enabled.
  //
  // Everything here degrades to the config values, because mozSettings is
  // absent in a desktop browser and MAY be refused on the phone: the Settings
  // API is permission-gated and a plain hosted app might not be granted it.
  // That's unverified on real hardware, so nothing depends on it working.
  var phoneProfile = { sound: null, vibrate: null }; // null = phone didn't say

  function readPhoneProfile() {
    var ms = navigator.mozSettings;
    if (!ms || !ms.createLock) return;
    try {
      var lock = ms.createLock();
      var vol = lock.get("audio.volume.notification");
      vol.onsuccess = function () {
        var v = vol.result["audio.volume.notification"];
        if (typeof v === "number") phoneProfile.sound = v > 0;
      };
      var vib = lock.get("vibration.enabled");
      vib.onsuccess = function () {
        var v = vib.result["vibration.enabled"];
        if (typeof v === "boolean") phoneProfile.vibrate = v;
      };
      // Follow the profile as it changes, so flipping the phone to silent
      // takes effect without restarting the app.
      if (ms.addObserver) {
        ms.addObserver("audio.volume.notification", function (e) {
          if (typeof e.settingValue === "number") phoneProfile.sound = e.settingValue > 0;
        });
        ms.addObserver("vibration.enabled", function (e) {
          if (typeof e.settingValue === "boolean") phoneProfile.vibrate = e.settingValue;
        });
      }
    } catch (e) { /* not permitted — the config values stand */ }
  }
  readPhoneProfile();

  // mode is "auto" | "always" | "never"; phoneSays is true/false/null.
  // "auto" follows the phone when it answers, and otherwise uses fallback —
  // which is why the default fallback for sound is OFF and for vibration is ON.
  // A missed buzz is a smaller problem than a dev build beeping in a meeting.
  function wants(mode, phoneSays, fallback) {
    if (mode === "always") return true;
    if (mode === "never") return false;
    return phoneSays === null ? fallback : phoneSays;
  }

  // appHidden is true when the app isn't the visible thing on screen — phone
  // shut, another app in front, screen off. Gecko 48 has document.hidden; the
  // fallback assumes visible, which errs toward the in-app bar rather than
  // firing an OS notification at someone staring at the chat.
  function appHidden() {
    if (typeof document.hidden === "boolean") return document.hidden;
    if (typeof document.mozHidden === "boolean") return document.mozHidden;
    return false;
  }

  // ---------- staying alive in the background ----------
  //
  // With push measured dead (see pushtest/), the app holding its own socket is
  // the only thing that delivers a message to this phone. So the app being
  // reaped a few seconds after you leave it isn't a nice-to-have — it's the
  // whole notification design failing. Two responses, in this order: record
  // what actually happens, and make the app a less attractive thing to kill.
  function wireLifecycle() {
    var on = CONFIG.KEEPALIVE !== false;
    Keepalive.setEnabled(on);

    // The audio element has to be unlocked by a user gesture, and going to the
    // background is not one. Any keypress will do, and there is always one
    // before the app can be backgrounded.
    document.addEventListener("keydown", function primeOnce() {
      Keepalive.prime();
      document.removeEventListener("keydown", primeOnce, false);
    }, false);

    function onVis() {
      if (appHidden()) Keepalive.start();
      else Keepalive.stop();
    }
    document.addEventListener("visibilitychange", onVis, false);
    document.addEventListener("mozvisibilitychange", onVis, false);
  }

  // osNotify raises a real system notification through the service worker.
  //
  // Tagged per chat and renotify:true, matching sw.js — a second message in the
  // same conversation updates that bubble and buzzes again, instead of stacking
  // a new one. The jid rides in data so sw.js's notificationclick handler opens
  // the right chat.
  function osNotify(title, body, jid) {
    // Deliberately minimal. On the phone the full option set produced a
    // vibration and no visible bubble, while Push Test's notification — which
    // passed body and nothing else — rendered and persisted. Gecko 48 is old
    // enough that an icon it can't resolve, or an option it doesn't implement,
    // can leave the notification unrendered without throwing anything.
    //
    // tag and data stay because they do real work: tag is what makes a second
    // message update that conversation's bubble instead of stacking a new one,
    // and data is how sw.js knows which chat to open when it's tapped. icon and
    // renotify are cosmetic and are the ones being dropped. KaiOS shows the
    // app's own icon anyway.
    var opts = {
      body: body || "",
      tag: "wa-" + jid,
      data: { jid: jid }
    };

    // Prefer the worker: only a worker notification survives the app being
    // closed, and only it can reopen the app when tapped.
    var reg = (window.App && window.App.registration) ? window.App.registration() : null;
    if (reg && reg.showNotification) {
      try {
        reg.showNotification(title || "Message", opts);
        return;
      } catch (e) { /* fall through */ }
    }

    // No worker yet. Registration is asynchronous and can fail outright, and
    // waiting on navigator.serviceWorker.ready is what made this silently do
    // nothing — that promise never resolves when nothing registered. A plain
    // page notification is worse (it dies with the page) but it is visible.
    if (window.Notification && Notification.permission === "granted") {
      try {
        new Notification(title || "Message", opts);
        return;
      } catch (e) { /* fall through */ }
    }
    toast("Can't show a notification: no worker, permission " +
      (window.Notification ? Notification.permission : "unsupported"));
  }

  function buzz() {
    var cfg = window.CONFIG || {};
    if (wants(cfg.NOTIFY_VIBRATE || "auto", phoneProfile.vibrate, true) &&
        navigator.vibrate) {
      try { navigator.vibrate(120); } catch (e) { /* not permitted; ignore */ }
    }
    if (wants(cfg.NOTIFY_SOUND || "auto", phoneProfile.sound, false)) {
      beep();
    }
  }

  // beep plays a short two-tone chirp, synthesised rather than shipped as an
  // audio file — no asset to load, no decoder to depend on, and it stays
  // audible on a small speaker.
  //
  // The context is built on first use: creating one at startup keeps the audio
  // hardware awake for a notification that may never come.
  var audioCtx = null;

  function beep() {
    try {
      if (!audioCtx) {
        var AC = window.AudioContext || window.webkitAudioContext;
        if (!AC) return;
        audioCtx = new AC();
        // Ask for the notification channel so the phone's own notification
        // volume (and silent mode) governs this, instead of media volume.
        // Privileged on Firefox OS, so it may be ignored — hence try/catch and
        // no reliance on it.
        try { audioCtx.mozAudioChannelType = "notification"; } catch (e) { /* fine */ }
      }
      // Desktop browsers start a context created without a user gesture in the
      // "suspended" state, which makes the beep silently do nothing.
      if (audioCtx.state === "suspended" && audioCtx.resume) audioCtx.resume();
      chirp(audioCtx.currentTime);
      chirp(audioCtx.currentTime + 0.13);
    } catch (e) { /* no audio available; the vibration carries it */ }
  }

  function chirp(at) {
    var osc = audioCtx.createOscillator();
    var gain = audioCtx.createGain();
    osc.type = "sine";
    osc.frequency.value = 880;
    // Ramp instead of switching on and off: an abrupt gain change is a click.
    gain.gain.setValueAtTime(0.0001, at);
    gain.gain.exponentialRampToValueAtTime(0.25, at + 0.01);
    gain.gain.exponentialRampToValueAtTime(0.0001, at + 0.09);
    osc.connect(gain);
    gain.connect(audioCtx.destination);
    osc.start(at);
    osc.stop(at + 0.1);
  }

  // alertForMessage decides what a newly arrived message does to the UI.
  function alertForMessage(m, c) {
    if (m.fromme) return;
    if (c && c.muted) return;   // a mute means silence, not a smaller icon

    if (currentJID === m.chat) {
      // You are reading this very chat.
      if (window.CONFIG && CONFIG.VIBRATE_IN_OPEN_CHAT) buzz();
      return;
    }

    // App backgrounded — the phone shut, or another app in front. An in-app bar
    // is invisible here, so raise a real OS notification instead.
    //
    // This needs no push service. A service worker may call showNotification()
    // for any reason, and the message arrived over the WebSocket we already
    // hold. KaiOS's push servers currently accept pushes and discard them, so
    // for now this is the ONLY route to a notification on this platform — and
    // it works exactly as long as the app stays running.
    if (appHidden()) {
      osNotify(
        (c && c.name) || m.chatname || m.chat,
        (c && c.preview) || "",
        m.chat
      );
      // Buzz separately rather than relying on the notification to do it: the
      // phone showed a vibration with no bubble, so the two clearly aren't the
      // same mechanism, and the buzz is the half you never want to lose.
      buzz();
      return;
    }

    buzz();
    if (!elList.hidden) return; // on the list; the badge says it better

    pendingNotif = {
      jid: m.chat,
      title: (c && c.name) || m.chatname || m.chat,
      body: (c && c.preview) || ""
    };
    showNotifBar();
    refreshNotifSoftkey();
  }

  function showNotifBar() {
    if (!pendingNotif) return;
    elNotifTitle.textContent = pendingNotif.title;
    elNotifBody.textContent = pendingNotif.body;
    elNotifBar.hidden = false;
    if (notifTimer) clearTimeout(notifTimer);
    notifTimer = setTimeout(hideNotifBar, NOTIF_HOLD_MS);
  }

  function hideNotifBar() {
    if (notifTimer) { clearTimeout(notifTimer); notifTimer = null; }
    elNotifBar.hidden = true;
  }

  // clearNotif drops the bar AND the pending notification, for when it stops
  // being news: you opened that chat, or went back to the list where you can
  // see everything anyway.
  function clearNotif() {
    hideNotifBar();
    pendingNotif = null;
  }

  // The left softkey is free now that the red key handles Back, so it becomes
  // "go to the chat that just messaged you" — and falls back to Back when
  // there's nothing waiting, since a dead softkey is worse than a plain one.
  function notifSoftLabel() {
    if (!pendingNotif) return "Back";
    return "🔔 " + truncate(pendingNotif.title, 8);
  }

  function checkNotif() {
    if (!pendingNotif) { enterListScreen(); return; }
    var jid = pendingNotif.jid;
    clearNotif();
    openThread(jid);
  }

  // Repaint the softkey when a notification arrives or is consumed — but only
  // while plain compose mode is what's on screen, so we never stomp on the
  // labels a menu or select mode has set.
  function refreshNotifSoftkey() {
    if (elThread.hidden || selectMode || menuOpen) return;
    if (!elChatMenu.hidden || !elMenu.hidden) return;
    Nav.setSoftkeys(notifSoftLabel(), "SEND", "•••");
  }

  // ---------- wire up events ----------
  var everConnected = false;

  W.onStatus(function (s) {
    elStatus.textContent =
      s === "open" ? "●" : s === "connecting" ? "…" : "○";
    elStatus.className = "status " + s;

    if (s !== "open") return;
    if (!everConnected) { everConnected = true; return; }

    // RECONNECT. The chat list refreshes on its own (wire asks for it), but an
    // already-open thread would sit there stale: openThread only fetches when
    // it has no history yet, and it isn't called again on reconnect. So
    // anything that arrived while we were disconnected — which is precisely
    // what a push notification wakes us for — stayed invisible until you left
    // the chat and came back. Re-pull the open thread explicitly.
    if (currentJID) {
      W.send(W.T.GETHISTORY, { jid: currentJID, before: 0, limit: 40 });
      W.send(W.T.MARKREAD, { jid: currentJID });
      if (!(chats[currentJID] && chats[currentJID].group)) {
        // Presence subscriptions are per-connection and don't survive.
        W.send(W.T.WATCH, { jid: currentJID });
      }
    }
  });

  W.on(W.T.READY, function () { console.log("daemon ready"); });

  W.on(W.T.QR, function (d) {
    // First-run pairing: the daemon sends a QR string. For the desktop test
    // you'll usually already be paired, so this may never fire. If it does,
    // we just show the code as text — rendering an actual QR on 240px is a
    // later nicety.
    show(elList);
    elList.innerHTML =
      '<div class="empty">Pair on phone: open WhatsApp &gt; Linked devices ' +
      'and scan the QR shown in the daemon console.</div>';
  });

  W.on(W.T.MESSAGE, function (d) { pushMsg(d); });

  W.on(W.T.HISTORY, function (d) {
    if (!d || !d.jid || !Array.isArray(d.messages)) return;
    var arr = threads[d.jid] = threads[d.jid] || [];
    // index existing by msgid to avoid duplicates
    var seen = {};
    for (var i = 0; i < arr.length; i++) seen[arr[i].msgid] = true;
    var added = 0;
    d.messages.forEach(function (m) {
      if (!seen[m.msgid]) { arr.push(m); seen[m.msgid] = true; added++; }
    });
    // sort oldest->newest by timestamp
    arr.sort(function (a, b) { return (a.ts || 0) - (b.ts || 0); });
    arr.loadedHistory = true;

    // Clear the in-flight flag on the reply itself rather than a timer — a slow
    // reply used to unblock early and let a second request pile on, while a
    // fast one left the flag set for the rest of the 300ms.
    var wasOlderRequest = (pendingOlderFor === d.jid);
    if (wasOlderRequest) {
      pendingOlderFor = null;
      loadingOlder = false;
      // Nothing came back: this chat's history is exhausted, so stop asking on
      // every subsequent keypress at the top.
      if (added === 0) arr.noMoreHistory = true;
    }

    // The array grew at the FRONT, so every index shifted. Re-anchor the
    // selection on the message it was actually pointing at.
    if (currentJID === d.jid && selectMode && selectMsgID) {
      for (var j = 0; j < arr.length; j++) {
        if (arr[j].msgid === selectMsgID) { selectIdx = j; break; }
      }
    }

    if (currentJID === d.jid && !menuOpen) {
      renderThread();
      // Keep the viewport over the same content after a prepend, or the reader
      // gets thrown to a random point in the newly inserted page.
      if (wasOlderRequest && added > 0 && prependAnchor && !selectMode) {
        elThreadMsgs.scrollTop = prependAnchor.top +
          (elThreadMsgs.scrollHeight - prependAnchor.height);
      }
    }
    prependAnchor = null;
    console.log("history:", d.jid, "+", added, "messages");
  });

  W.on(W.T.CHATLIST, function (list) {
    if (Array.isArray(list)) {
      list.forEach(function (c) {
        var ex = chats[c.jid] || {};
        // merge daemon record over any existing, keeping newest ts
        chats[c.jid] = {
          jid: c.jid,
          name: c.name || ex.name || c.jid,
          group: (typeof c.group === "boolean") ? c.group : ex.group,
          pinned: (typeof c.pinned === "boolean") ? c.pinned : ex.pinned,
          muted: (typeof c.muted === "boolean") ? c.muted : ex.muted,
          archived: (typeof c.archived === "boolean") ? c.archived : ex.archived,
          ts: Math.max(c.ts || 0, ex.ts || 0),
          preview: c.preview || ex.preview || "",
          // The daemon derives this from messages we never marked read, so it
          // is the authority — a local counter would reset on every refresh,
          // which is exactly the bug this replaces.
          unread: (typeof c.unread === "number") ? c.unread : (ex.unread || 0)
        };
      });
      renderChatList(); // always render — list may be the visible screen after refresh
    }
  });

  // presence: jid -> {online, lastseen}
  var presence = {};

  W.on(W.T.PRESENCE, function (d) {
    if (!d || !d.jid) return;
    presence[d.jid] = { online: !d.unavailable, lastseen: d.lastseen || 0 };
    if (currentJID === d.jid) refreshTypingUI(d.jid);
  });

  W.on(W.T.TYPING, function (d) {
    if (!d || !d.chat || !d.sender) return;
    setTyping(d.chat, d.sender, d.sendername, d.state === "composing");
  });

  // The daemon holds the authoritative end time for a live share — its clock
  // keeps running while the phone's timers are throttled. When it says the
  // share is over, stop producing fixes, because that's what turns the GPS off.
  W.on(W.T.LIVELOCSTATE, function (d) {
    if (!d || !d.chat) return;
    if (!d.active) {
      if (liveShare.active && liveShare.chat === d.chat) {
        stopLiveLocation("Live location ended");
      }
      return;
    }
    if (liveShare.active && liveShare.chat === d.chat && d.until) {
      liveShare.endsAt = d.until * 1000;
      paintLiveBar();
    }
  });

  W.on(W.T.EDITED, function (d) {
    if (!d || !d.chat || !d.msgid) return;
    var arr = threads[d.chat] || [];
    for (var i = 0; i < arr.length; i++) {
      if (arr[i].msgid === d.msgid) {
        arr[i].text = d.text;
        arr[i].edited = true;
        break;
      }
    }
    // An existing bubble changed, so the append-only path can't be used.
    forceRerender();
    if (currentJID === d.chat && !menuOpen) renderThread();
    else if (!elList.hidden) renderChatList();
  });

  W.on(W.T.REACTION, function (d) {
    if (!d || !d.chat || !d.msgid) return;
    var arr = threads[d.chat] || [];
    for (var i = 0; i < arr.length; i++) {
      if (arr[i].msgid === d.msgid) {
        // The daemon sends the complete set, so replace rather than merge —
        // that way removals land correctly too.
        arr[i].reactions = d.reactions || [];
        break;
      }
    }
    if (currentJID === d.chat && !menuOpen) renderThread();
  });

  W.on(W.T.STATUS, function (d) {
    if (!d || !d.chat || !d.msgid) return;
    var arr = threads[d.chat] || [];
    for (var i = 0; i < arr.length; i++) {
      if (arr[i].msgid === d.msgid) { arr[i].status = d.status; break; }
    }
    if (currentJID === d.chat && !menuOpen) renderThread();
  });

  W.on(W.T.SEARCHRESULT, function (d) {
    if (!elSearch.hidden) renderSearchResults(d);
  });

  W.on(W.T.PROFILE, function (p) {
    if (!p || !p.jid) return;
    var req = pendingProfile;
    pendingProfile = null;
    // A "dm" request only wanted the resolved JID so it could open the chat;
    // seed the chat record from the profile so it has a name straight away.
    if (req && req.mode === "dm") {
      var c = chats[p.jid] = chats[p.jid] || { jid: p.jid, name: p.name || p.jid };
      c.name = p.name || c.name;
      c.group = !!p.group;
      openThread(p.jid);
      return;
    }
    if (chats[p.jid] && p.name) chats[p.jid].name = p.name;
    renderProfile(p);
  });

  W.on(W.T.CHATUPDATE, function (d) {
    if (!d || !d.chat) return;
    if (d.removed) {
      delete chats[d.chat];
      delete threads[d.chat];
      if (currentJID === d.chat) enterListScreen();
      else renderChatList();
      return;
    }
    var c = chats[d.chat];
    if (!c) return;
    if (typeof d.pinned === "boolean") c.pinned = d.pinned;
    if (typeof d.muted === "boolean") c.muted = d.muted;
    if (typeof d.archived === "boolean") c.archived = d.archived;
    if (typeof d.unread === "number") c.unread = d.unread;
    if (!elList.hidden) renderChatList();
  });

  W.on(W.T.CALLOFFER, function (d) { showIncomingCall(d); });

  W.on(W.T.CALLSTATE, function (d) {
    console.log("call state:", d.state, d.reason || "");
    if (d.state === "ended") { activeCall = null; if (elCall.hidden === false) enterListScreen(); }
  });

  W.on(W.T.ERROR, function (d) {
    console.error("daemon error:", d.code, d.msg);
    // A failed send used to look exactly like a successful one. Say something.
    toast(friendlyError(d));
  });

  // friendlyError turns a daemon error code into something worth reading on a
  // 240px screen. Unknown codes fall back to the raw message, which is still
  // better than silence.
  function friendlyError(d) {
    if (!d) return "Something went wrong";
    switch (d.code) {
      case "send": return "Message not sent";
      case "sendmedia": return "Attachment not sent";
      case "chataction": return "Couldn't change that chat";
      case "delete": return "Couldn't delete";
      case "forward": return "Couldn't forward";
      case "reaction": return "Couldn't react";
      case "profile": return "Couldn't load profile";
    }
    return (d.msg || "Something went wrong").slice(0, 60);
  }

  // Show how many messages are waiting to go out while offline.
  W.onQueued(function (n) {
    if (n > 0) toast(n + " message" + (n === 1 ? "" : "s") + " waiting to send");
  });

  W.on("deleted", function (d) {
    // someone (or we) deleted a message; mark it if we have it
    var arr = threads[d.chat] || [];
    for (var i = 0; i < arr.length; i++) {
      if (arr[i].msgid === d.msgid) { arr[i].deleted = true; break; }
    }
    if (currentJID === d.chat && !menuOpen) renderThread();
  });

  // ---------- boot ----------
  document.getElementById("reply-bar-x").onclick = clearReply;

  // Typing signal comes off real input events, not keydown, so D-pad and
  // softkey presses don't register as composing.
  elInput.addEventListener("input", noteTyping);
  elInput.addEventListener("input", stashDraft);
  elSearchInput.addEventListener("input", scheduleSearch);

  // Scrolling to the top of a thread loads an older page. D-pad navigation goes
  // through moveSelect instead, which calls the same helper — scrollIntoView
  // doesn't reliably fire this listener, which is why selecting upwards used to
  // stop dead while dragging the scrollbar worked.
  elThreadMsgs.addEventListener("scroll", function () {
    if (elThreadMsgs.scrollTop > 8) return;
    requestOlderHistory();
  });
  // A small surface for push.js: tapping a notification should open that chat.
  window.App = {
    openChat: function (jid) {
      if (!jid) return;
      if (!chats[jid]) chats[jid] = { jid: jid, name: jid };
      openThread(jid);
    },
    // Dev hooks. Alerting is the one part that can't be checked by reading the
    // code — whether the phone answers mozSettings, whether the beep is audible
    // over a speaker — so make it triggerable on demand.
    testAlert: function () { buzz(); },
    testBeep: function () { beep(); },
    alertProfile: function () {
      return {
        phoneSays: phoneProfile,
        mozSettings: !!navigator.mozSettings,
        canVibrate: !!navigator.vibrate
      };
    },
    version: versionLabel,
    // Push a fake INCOMING message through the same path a real one takes, so
    // the notification decision, the buzz and the bar are all exercised.
    // Messaging yourself can't test this: those arrive as fromme, which is
    // skipped on purpose.
    testIncoming: function (name, text) {
      pushMsg({
        chat: "testincoming@s.whatsapp.net",
        chatname: name || "Test contact",
        msgid: "test-" + Date.now(),
        sender: "testincoming@s.whatsapp.net",
        sendername: name || "Test contact",
        fromme: false,
        ts: Math.floor(Date.now() / 1000),
        kind: "text",
        text: text || "Testing an incoming message"
      });
      return "sent — if the app is backgrounded you should get a notification";
    },
    testBanner: function (title, body) {
      pendingNotif = { jid: currentJID || "test@s.whatsapp.net",
                       title: title || "Test chat", body: body || "A message" };
      showNotifBar();
      refreshNotifSoftkey();
    }
  };

  // ---------- first-run setup ----------
  //
  // Where the daemon lives is the one thing the app can't guess and can't ship
  // baked in: a LAN address changes with DHCP, and a token in the package would
  // travel inside every zip uploaded to a submission portal. So ask once, store
  // it on the phone, and offer a way back in when the address moves.
  function enterSetupScreen(returnTo) {
    var cur = Settings.current();
    document.querySelector("#setup .setup-head").textContent =
      "Connect to your daemon — " + versionLabel();
    elSetupHost.value = cur.host || "";
    elSetupToken.value = cur.token || "";
    updateSetupPreview();
    updateSetupDiag();
    show(elSetup);

    var field = 0;                       // 0 = host, 1 = token
    var fields = [elSetupHost, elSetupToken];
    function focusField(i) {
      field = (i + fields.length) % fields.length;
      fields.forEach(function (f) { f.className = ""; });
      fields[field].className = "focused";
      fields[field].focus();
    }

    function saveAndGo() {
      var saved = Settings.save(elSetupHost.value, elSetupToken.value);
      if (!saved) { toast("Enter the server address"); return; }
      // Reconnect against the new address rather than waiting for a retry —
      // the old socket is pointed at somewhere that may no longer exist.
      // Let go of the field before the screen changes; by the time
      // enterListScreen runs, the input method has already been handed the
      // keypad for this frame.
      try { elSetupHost.blur(); elSetupToken.blur(); } catch (e) {}
      W.reconnect();
      enterListScreen();
    }

    Nav.setScreen({
      onUp: function () { focusField(field - 1); return true; },
      onDown: function () { focusField(field + 1); return true; },
      onEnter: saveAndGo,
      onSoftRight: saveAndGo,
      // No way out on first run: without an address there's nothing to show.
      // Left is the notification test rather than Cancel: the red key already
      // goes back, and a notification you cannot trigger is one you cannot
      // debug without a console you don't have.
      onSoftLeft: testNotification,
      onBack: returnTo || null
    });
    Nav.setSoftkeys("Test notif", "SAVE", "Save");
    focusField(0);
  }

  // The running build's version, stamped in at package time. Answers "did the
  // update actually land" without guessing from behaviour.
  function versionLabel() {
    return window.KAITS_VERSION ? "Kaits " + window.KAITS_VERSION : "Kaits (dev)";
  }

  // Everything that decides whether a notification can appear, in the only
  // place it can be read on a device with no devtools.
  function updateSetupDiag() {
    if (!elSetupDiag) return;
    var lines = [];
    // First, because everything below it depends on it. Service workers and
    // the Notification API are unavailable outside a secure context, and the
    // failures are silent: permission requests reject, registration never
    // happens, and nothing says why. localhost counts as secure; a bare LAN IP
    // over plain http does not.
    var secure = (typeof isSecureContext === "boolean")
      ? isSecureContext
      : (location.protocol === "https:" || location.protocol === "app:" ||
         location.hostname === "localhost" || location.hostname === "127.0.0.1");
    if (!secure) {
      lines.push("!! INSECURE ORIGIN (" + location.origin + ")");
      lines.push("   notifications need https, localhost, or the packaged app");
    }
    lines.push("socket: " + (W.isOpen() ? "connected" : "not connected"));
    lines.push("notifications: " +
      (window.Notification ? Notification.permission : "unsupported"));
    var reg = (window.App && window.App.registration) ? window.App.registration() : null;
    lines.push("service worker: " +
      (!("serviceWorker" in navigator) ? "unsupported"
        : reg ? "registered" : "NOT registered"));
    var ps = (window.App && window.App.pushState) ? window.App.pushState() : null;
    if (ps) lines.push("push: " + ps.state);

    // How long the app survived last time it was out of sight, and whether
    // that's a pattern. This is the only place the answer can appear: an app
    // that gets killed doesn't get to report anything at the time.
    if (window.Life) {
      lines.push(Life.describe());
      var kr = Life.killRate();
      if (kr) lines.push("background kills: " + kr.killed + " of last " + kr.total);
      var ka = Keepalive.state();
      lines.push("keepalive: " + (!ka.wanted ? "off"
        : ka.running ? "playing" : ka.primed ? "primed" : "not primed yet") +
        (ka.error ? " (" + ka.error + ")" : ""));
      var hist = Life.report();
      if (hist.length) lines.push("— sessions, newest first —");
      for (var i = 0; i < hist.length && i < 8; i++) lines.push("  " + hist[i]);
    }
    lines.push(versionLabel());
    elSetupDiag.textContent = lines.join("\n");
  }

  // A notification raised the same way a real message would raise one, so a
  // failure here and a failure on an incoming message have the same cause.
  function testNotification() {
    if (!window.Notification) { toast("No Notification API here"); return; }
    if (Notification.permission !== "granted" && Notification.requestPermission) {
      var req;
      try {
        req = Notification.requestPermission();
      } catch (e) {
        toast("Permission request failed: " + e.message);
        return;
      }
      // Outside a secure context this rejects, and awaiting it silently was
      // exactly how "nothing happens and nothing says why" came about.
      Promise.resolve(req).then(function (p) {
        updateSetupDiag();
        if (p !== "granted") { toast("Permission " + p); return; }
        osNotify("Kaits", "Test notification", "test@s.whatsapp.net");
      }, function (e) {
        updateSetupDiag();
        toast("Can't ask for permission: " + (e && e.message ? e.message : e));
      });
      return;
    }
    osNotify("Kaits", "Test notification", "test@s.whatsapp.net");
    buzz();
    toast("Sent — close the flip and check");
  }

  function updateSetupPreview() {
    var url = Settings.preview(elSetupHost.value);
    elSetupPreview.textContent = url ? "→ " + url : "";
  }
  elSetupHost.addEventListener("input", updateSetupPreview);

  window.App.setup = function () { enterSetupScreen(enterListScreen); };

  window.App.life = function () {
    return {
      previous: Life.previous(),
      killRate: Life.killRate(),
      sessions: Life.report(),
      keepalive: Keepalive.state()
    };
  };
  window.App.forgetLife = function () { Life.clear(); return "cleared"; };

  // Start the recorder before anything else can fail: a session that dies
  // during boot is exactly the kind we want on the record.
  Life.start(window.KAITS_VERSION || "dev");
  wireLifecycle();

  // Boot. An unconfigured app has nowhere to connect, so it asks first and
  // connects afterwards.
  if (Settings.configured()) {
    enterListScreen();
    W.connect();
  } else {
    enterSetupScreen(null);
  }
})();
