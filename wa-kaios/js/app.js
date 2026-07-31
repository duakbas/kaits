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
  var elInput = document.getElementById("composer");
  var elAttach = document.getElementById("attach-btn");
  var elReplyBar = document.getElementById("reply-bar");
  var elReplyBarText = document.getElementById("reply-bar-text");
  var elMenu = document.getElementById("action-menu");
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
  var elSearchInput = document.getElementById("search-input");
  var elSearchResults = document.getElementById("search-results");

  function show(el) {
    [elList, elThread, elCall, elProfile, elSearch].forEach(function (s) { s.hidden = true; });
    el.hidden = false;
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

  function renderChatList() {
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
    scheduleAvatarLoad();
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

  function enterListScreen() {
    stashDraft();
    stopTyping();
    currentJID = null;
    show(elList);
    Nav.setScreen({
      list: elList,
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
        if (!el) return;
        if (el.getAttribute("data-archived")) { openArchived(); return; }
        if (el.getAttribute("data-archived-back")) { closeArchived(); return; }
        openThread(el.getAttribute("data-jid"));
      },
      onSoftLeft: enterSearchScreen,
      // Right on a chat row opens its pin/mute/archive/delete menu. The right
      // softkey does the same, since Right isn't discoverable on its own.
      onRight: function () {
        var el = Nav.focusedEl();
        if (el) openChatMenu(el.getAttribute("data-jid"));
      },
      onSoftRight: function () {
        var el = Nav.focusedEl();
        if (el) openChatMenu(el.getAttribute("data-jid"));
      }
    });
    Nav.setSoftkeys("Search", "SELECT", "Options");
    renderChatList();
  }

  // ---------- chat action menu (pin / mute / archive / delete / info) ----------
  // These are real WhatsApp account changes, not local preferences: they sync
  // to the phone and every other linked device. Delete is not undoable, so it
  // asks for confirmation first.
  var chatMenuJID = null;

  function openChatMenu(jid) {
    if (!jid) return;
    var c = chats[jid] || { jid: jid };
    chatMenuJID = jid;
    elChatMenuTitle.textContent = c.name || jid;
    elChatMenuList.innerHTML = "";
    [
      { action: "pin", label: c.pinned ? "Unpin" : "Pin" },
      { action: "mute", label: c.muted ? "Unmute" : "Mute" },
      { action: "archive", label: c.archived ? "Unarchive" : "Archive" },
      { action: "info", label: c.group ? "Group info" : "Contact info" },
      { action: "delete", label: "Delete chat" }
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
    enterListScreen();
  }

  function runChatAction(action) {
    var jid = chatMenuJID;
    if (!jid) { closeChatMenu(); return; }
    var c = chats[jid] || {};
    if (action === "info") {
      elChatMenu.hidden = true;
      openProfile(jid, enterListScreen);
      return;
    }
    if (action === "delete") {
      elChatMenu.hidden = true;
      confirmPrompt("Delete chat?",
        "Deletes it on WhatsApp and every linked device. Cannot be undone.",
        function () { W.send(W.T.CHATACTION, { chat: jid, action: "delete" }); enterListScreen(); },
        function () { openChatMenu(jid); });
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
    var c = chats[jid] || { jid: jid, name: jid };
    // Clear optimistically; the daemon confirms with a chatupdate once the read
    // receipt has actually gone out.
    c.unread = 0;
    // Drop any paging state belonging to the chat we just left, or a reply
    // still in flight for it would keep this chat from loading older pages.
    loadingOlder = false;
    pendingOlderFor = null;
    prependAnchor = null;
    elThreadTitle.textContent = c.name || jid;
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
    renderThread();
    restoreDraft(jid);
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
      onSoftLeft: enterListScreen,    // "Back"
      onSoftRight: sendCurrent,       // "Send"
      onEnter: sendCurrent
    });
    Nav.setSoftkeys("Back", "📎 Left", "Send");
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
      onEnter: openActionMenu,
      onSoftLeft: enterComposeMode,   // "Cancel" back to typing
      onSoftRight: openActionMenu,    // "Actions"
      onBack: enterComposeMode
    });
    Nav.setSoftkeys("Cancel", "", "Actions");
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
    if (prev === next) { if (next) next.scrollIntoView({ block: "nearest" }); return; }
    if (prev) prev.className = prev.className.replace(/ ?\bselected\b/, "");
    if (next) {
      next.className += " selected";
      next.scrollIntoView({ block: "nearest" });
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
    [
      { action: "photo", label: "📷  Photo" },
      { action: "file", label: "📎  File" }
    ].forEach(function (it) {
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
        pickAndSend(el.getAttribute("data-attach") === "photo" ? "image" : "doc");
        enterComposeMode();
      },
      onSoftLeft: close, onBack: close,
      onSoftRight: function () {
        var el = Nav.focusedEl();
        if (el) {
          elChatMenu.hidden = true;
          pickAndSend(el.getAttribute("data-attach") === "photo" ? "image" : "doc");
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
    Nav.setScreen({
      list: elMenu,
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
    if (currentJID === chat) {
      // Typing outranks presence: it's the more immediate fact.
      var label = typingLabel(chat) || presenceLabel(chat);
      var c = chats[chat] || {};
      elThreadTitle.textContent = label ? (c.name || chat) + " — " + label : (c.name || chat);
    }
    if (!elList.hidden) renderChatList();
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

  function renderMediaBubble(b, m) {
    var url = mediaBase() + (m.media || "");
    if (m.kind === "image") {
      var img = document.createElement("img");
      img.className = "media-img";
      img.src = url;
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
      g.src = url;
      g.autoplay = true;
      g.loop = true;
      g.muted = true;
      g.setAttribute("muted", "");     // some engines need the attribute too
      g.setAttribute("playsinline", "");
      b.appendChild(g);
      // nudge autoplay in case the attribute alone doesn't trigger it
      g.play && g.play().catch(function () {});
    } else if (m.kind === "video") {
      var vid = document.createElement("video");
      vid.className = "media-img";
      vid.src = url;
      vid.controls = true;
      vid.preload = "metadata";
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
      stk.src = url;
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
      if (sel) sel.scrollIntoView({ block: "nearest" });
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
    Nav.setSoftkeys("Back", "", "OK");
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

  // ---------- sending attachments ----------
  // The bytes come from the phone's own Camera/Gallery via a pick activity,
  // which is the only way to reach them — an app can't browse the filesystem.
  // On desktop there's a hidden file input instead, so this is testable.
  function pickAndSend(kind) {
    if (!currentJID) return;
    if (window.MozActivity) {
      try {
        var act = new window.MozActivity({
          name: "pick",
          data: { type: kind === "image" ? ["image/jpeg", "image/png"] : ["*/*"] }
        });
        act.onsuccess = function () {
          var blob = this.result && this.result.blob;
          if (blob) blobToBase64AndSend(blob, kind, this.result.name || "");
          else toast("Nothing picked");
        };
        act.onerror = function () { toast("Couldn't open the picker"); };
        return;
      } catch (e) { /* fall through to the desktop path */ }
    }
    desktopFilePick(kind);
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
      onSoftLeft: enterListScreen,
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
    Nav.setSoftkeys("Back", "", "Search");
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
    }
  };

  enterListScreen();
  W.connect();
})();
