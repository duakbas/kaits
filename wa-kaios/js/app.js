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
  var replyingTo = null;  // {msgid, name, text} when composing a reply
  var menuOpen = false;   // action menu overlay visible
  var forceScrollBottom = false; // force thread to bottom on next render (open)

  // ---- element refs ----
  var elList = document.getElementById("chatlist");
  var elThread = document.getElementById("thread");
  var elThreadMsgs = document.getElementById("thread-msgs");
  var elThreadTitle = document.getElementById("thread-title");
  var elInput = document.getElementById("composer");
  var elReplyBar = document.getElementById("reply-bar");
  var elReplyBarText = document.getElementById("reply-bar-text");
  var elMenu = document.getElementById("action-menu");
  var elCall = document.getElementById("call");
  var elCallName = document.getElementById("call-name");
  var elStatus = document.getElementById("status");

  function show(el) {
    [elList, elThread, elCall].forEach(function (s) { s.hidden = true; });
    el.hidden = false;
  }

  // ---------- chat list screen ----------
  function renderChatList() {
    var arr = Object.keys(chats).map(function (j) { return chats[j]; });
    arr.sort(function (a, b) {
      // pinned chats first, then by recency
      if (!!a.pinned !== !!b.pinned) return a.pinned ? -1 : 1;
      return (b.ts || 0) - (a.ts || 0);
    });

    elList.innerHTML = "";
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
      var initials = (c.name || c.jid || "?").replace(/[^A-Za-z0-9 ]/g, "")
        .trim().split(/\s+/).slice(0, 2)
        .map(function (w) { return w.charAt(0).toUpperCase(); }).join("") || "?";
      av.textContent = initials;
      av.style.background = colorFor(c.name || c.jid);
      av.setAttribute("data-avatar-jid", c.jid); // observer picks this up

      var col = document.createElement("div");
      col.className = "chat-text";
      var name = document.createElement("div");
      name.className = "chat-name";
      name.textContent = (c.pinned ? "📌 " : "") + (c.name || c.jid);
      var prev = document.createElement("div");
      prev.className = "chat-prev";
      prev.textContent = c.preview || "";
      col.appendChild(name);
      col.appendChild(prev);

      row.appendChild(av);
      row.appendChild(col);
      if (c.unread) {
        var dot = document.createElement("span");
        dot.className = "unread";
        row.appendChild(dot);
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

  function enterListScreen() {
    currentJID = null;
    show(elList);
    Nav.setScreen({
      list: elList,
      onEnter: function (e, el) { if (el) openThread(el.getAttribute("data-jid")); }
    });
    Nav.setSoftkeys("", "SELECT", "");
    renderChatList();
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
    c.unread = 0;
    elThreadTitle.textContent = c.name || jid;
    show(elThread);
    clearReply();
    forceScrollBottom = true;
    // ask the daemon for stored history for this chat (latest page)
    if (!threads[jid] || !threads[jid].loadedHistory) {
      W.send(W.T.GETHISTORY, { jid: jid, before: 0, limit: 40 });
    }
    renderThread();
    elInput.value = "";
    enterComposeMode();
  }

  function enterComposeMode() {
    selectMode = false;
    selectIdx = -1;
    renderThread(); // clear any bubble highlight
    Nav.setScreen({
      onUp: enterSelectMode,          // arrow up starts selecting messages
      onBack: enterListScreen,
      onSoftLeft: enterListScreen,    // "Back"
      onSoftRight: sendCurrent,       // "Send"
      onEnter: sendCurrent
    });
    Nav.setSoftkeys("Back", "", "Send");
    setTimeout(function () { elInput.focus(); }, 0);
  }

  function enterSelectMode() {
    var msgs = threads[currentJID] || [];
    if (!msgs.length) return;
    elInput.blur();
    selectMode = true;
    selectIdx = msgs.length - 1; // start at the newest message
    renderThread();
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
    if (next < 0) { selectIdx = 0; renderThread(); return; }
    if (next >= msgs.length) { enterComposeMode(); return; } // down off the end
    selectIdx = next;
    renderThread();
  }

  // ---------- action menu ----------
  function openActionMenu() {
    var msgs = threads[currentJID] || [];
    if (selectIdx < 0 || selectIdx >= msgs.length) return;
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
    enterSelectMode(); // return to selecting, keeping position
    selectIdx = selectIdx; // (kept)
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
    } else if (action === "delete") {
      // Only own messages can be deleted for everyone.
      if (!m.fromme) {
        console.warn("can only delete your own messages");
        closeActionMenu();
        return;
      }
      W.send(W.T.DELETE, { chat: currentJID, msgid: m.msgid });
      // optimistic: mark locally as deleted
      m.deleted = true;
      menuOpen = false;
      elMenu.hidden = true;
      renderThread();
      enterComposeMode();
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
    }
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
    } else if (m.kind === "doc") {
      var doc = document.createElement("div");
      doc.className = "media-file";
      doc.textContent = "📄 " + (m.text || "Document");
      b.appendChild(doc);
    } else {
      b.textContent = "[" + m.kind + "]";
    }
  }

  function renderThread() {
    var msgs = threads[currentJID] || [];
    var isGroup = chats[currentJID] && chats[currentJID].group;
    // Was the user near the bottom BEFORE we rebuild? If so we'll auto-scroll
    // after; if they'd scrolled up to read history, we leave them there.
    var wasNearBottom = forceScrollBottom || (elThreadMsgs.scrollHeight - elThreadMsgs.scrollTop
      - elThreadMsgs.clientHeight) < 60;
    forceScrollBottom = false;
    elThreadMsgs.innerHTML = "";
    var lastSender = null;
    msgs.forEach(function (m, idx) {
      // In groups, label each incoming message with its sender — but only when
      // the sender changes, so runs from one person aren't repetitive.
      if (isGroup && !m.fromme && m.sendername && m.sendername !== lastSender) {
        var who = document.createElement("div");
        who.className = "sender";
        who.textContent = m.sendername;
        who.style.color = colorFor(m.sendername);
        elThreadMsgs.appendChild(who);
      }
      lastSender = m.fromme ? null : m.sendername;

      var b = document.createElement("div");
      b.className = "bubble " + (m.fromme ? "me" : "them");
      // mark the selected bubble in select mode
      if (selectMode && idx === selectIdx) b.className += " selected";
      // tint incoming group bubbles' left border with the sender color
      if (isGroup && !m.fromme && m.sendername) {
        b.style.borderLeft = "2px solid " + colorFor(m.sendername);
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
      elThreadMsgs.appendChild(b);
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
    W.send(W.T.SEND, frame);
    // optimistic local echo (include the quote preview so it shows immediately)
    var echo = {
      msgid: "local-" + Date.now(), chat: currentJID, fromme: true,
      ts: Math.floor(Date.now() / 1000), kind: "text", text: text
    };
    if (replyingTo) {
      echo.quotedtext = replyingTo.text;
      echo.quotedname = replyingTo.name;
    }
    pushMsg(echo);
    elInput.value = "";
    clearReply();
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
    if (!m.fromme && currentJID !== m.chat) c.unread = (c.unread || 0) + 1;

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
  W.onStatus(function (s) {
    elStatus.textContent =
      s === "open" ? "●" : s === "connecting" ? "…" : "○";
    elStatus.className = "status " + s;
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
    if (currentJID === d.jid && !menuOpen) renderThread();
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
          ts: Math.max(c.ts || 0, ex.ts || 0),
          preview: c.preview || ex.preview || "",
          unread: ex.unread || 0
        };
      });
      renderChatList(); // always render — list may be the visible screen after refresh
    }
  });

  W.on(W.T.CALLOFFER, function (d) { showIncomingCall(d); });

  W.on(W.T.CALLSTATE, function (d) {
    console.log("call state:", d.state, d.reason || "");
    if (d.state === "ended") { activeCall = null; if (elCall.hidden === false) enterListScreen(); }
  });

  W.on(W.T.ERROR, function (d) {
    console.error("daemon error:", d.code, d.msg);
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

  // scroll to top of a thread -> load an older page of history
  var loadingOlder = false;
  elThreadMsgs.addEventListener("scroll", function () {
    if (elThreadMsgs.scrollTop > 8 || loadingOlder || !currentJID) return;
    var arr = threads[currentJID] || [];
    if (!arr.length) return;
    var oldestTS = arr[0].ts || 0;
    if (!oldestTS) return;
    loadingOlder = true;
    var prevHeight = elThreadMsgs.scrollHeight;
    W.send(W.T.GETHISTORY, { jid: currentJID, before: oldestTS, limit: 40 });
    // keep scroll position stable after older messages prepend
    setTimeout(function () {
      var newHeight = elThreadMsgs.scrollHeight;
      elThreadMsgs.scrollTop += (newHeight - prevHeight);
      loadingOlder = false;
    }, 300);
  });
  enterListScreen();
  W.connect();
})();
