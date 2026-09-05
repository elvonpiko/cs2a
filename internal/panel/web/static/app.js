// cs2a panel glue: styled confirm modal, toasts, htmx niceties.
(function () {
	"use strict";

	// --- styled confirm modal (replaces window.confirm) --------------------
	// askConfirm(message, {danger}) -> Promise<boolean>
	function askConfirm(message, opts) {
		opts = opts || {};
		return new Promise(function (resolve) {
			var lastFocus = document.activeElement;
			var overlay = document.createElement("div");
			overlay.className = "modal-overlay";
			overlay.innerHTML =
				'<div class="modal-card' + (opts.danger ? " danger" : "") + '" role="alertdialog" aria-modal="true">' +
				'  <div class="modal-icon">' + (opts.danger ? "!" : "?") + "</div>" +
				"  <h3>" + (opts.title || (opts.danger ? "Are you sure?" : "Confirm")) + "</h3>" +
				"  <p></p>" +
				'  <div class="modal-actions">' +
				'    <button class="btn ghost" data-modal="cancel" type="button">Cancel</button>' +
				'    <button class="btn ' + (opts.danger ? "danger" : "primary") + '" data-modal="ok" type="button">' + (opts.okLabel || "Confirm") + "</button>" +
				"  </div>" +
				"</div>";
			overlay.querySelector("p").textContent = message;
			document.body.appendChild(overlay);

			function done(val) {
				overlay.remove();
				document.removeEventListener("keydown", onKey);
				if (lastFocus && lastFocus.focus) lastFocus.focus();
				resolve(val);
			}
			// Escape cancels. Enter is deliberately NOT handled: the focused
			// button already activates on Enter, and a document-level handler
			// confirmed the action even when the user had tabbed to Cancel.
			function onKey(e) {
				if (e.key === "Escape") {
					e.preventDefault();
					done(false);
					return;
				}
				if (e.key !== "Tab") return;
				// Keep focus inside the dialog: tabbing out of it left the
				// operator typing into the page behind a modal overlay.
				var focusable = overlay.querySelectorAll("[data-modal]");
				if (!focusable.length) return;
				var first = focusable[0];
				var last = focusable[focusable.length - 1];
				if (e.shiftKey && document.activeElement === first) {
					e.preventDefault();
					last.focus();
				} else if (!e.shiftKey && document.activeElement === last) {
					e.preventDefault();
					first.focus();
				}
			}
			overlay.addEventListener("click", function (e) {
				var b = e.target.closest("[data-modal]");
				if (b) return done(b.getAttribute("data-modal") === "ok");
				if (e.target === overlay) done(false);
			});
			document.addEventListener("keydown", onKey);
			var okBtn = overlay.querySelector('[data-modal="ok"]');
			if (okBtn) okBtn.focus();
		});
	}
	window.cs2aConfirm = askConfirm;

	// Run the confirm dialog for `btn` and submit its form when accepted.
	function guard(btn) {
		var msg = btn.getAttribute("data-confirm");
		var danger = btn.classList.contains("danger") || btn.hasAttribute("data-confirm-danger");
		var form = btn.closest("form");
		askConfirm(msg, {
			danger: danger,
			okLabel: danger ? "Yes, do it" : "Confirm",
			title: danger ? "This will affect the live server" : "Confirm action"
		}).then(function (yes) {
			if (!yes) return;
			btn.removeAttribute("data-confirm");
			if (form && form.requestSubmit) form.requestSubmit(btn);
			else if (form) form.submit();
			else btn.click();
		});
	}

	// intercept every data-confirm action (submit buttons + plain buttons)
	document.addEventListener("click", function (ev) {
		var btn = ev.target.closest("[data-confirm]");
		if (!btn) return;
		ev.preventDefault();
		ev.stopPropagation();
		guard(btn);
	}, true);

	// A click handler alone is not enough: a form can be submitted by pressing
	// Enter in a field, which in some browsers never produces a click on the
	// button. The map-change form is the one guarded form with a focusable
	// field, so Enter in the map <select> could switch the live map with no
	// confirmation. This is the authoritative gate; the click handler above only
	// makes the dialog appear before the button's default action.
	document.addEventListener("submit", function (ev) {
		var form = ev.target;
		if (!form || form.tagName !== "FORM") return;
		// Prefer the actual submitter so a form with several buttons cannot show
		// the wrong message; fall back to scanning for implicit submissions that
		// report no submitter.
		var btn = ev.submitter;
		if (btn && !btn.hasAttribute("data-confirm")) return;
		if (!btn) btn = form.querySelector("[data-confirm]");
		if (!btn || !btn.hasAttribute("data-confirm")) return;
		ev.preventDefault();
		ev.stopPropagation();
		guard(btn);
	}, true);

	// --- toast helper -------------------------------------------------------
	var toastTimer = null;
	function toast(msg, kind) {
		var el = document.getElementById("toast");
		if (!el) return;
		el.textContent = msg;
		el.className = "show " + (kind || "");
		clearTimeout(toastTimer);
		toastTimer = setTimeout(function () { el.className = ""; }, 3400);
	}
	window.cs2aToast = toast;

	// surface htmx errors as toasts
	document.body.addEventListener("htmx:responseError", function () {
		toast("Something went wrong — check the server logs.", "err");
	});
	document.body.addEventListener("htmx:sendError", function () {
		toast("Lost connection to the panel.", "err");
	});

	// Auto-dismiss flash notices — successes only. An error flash is often the
	// only place a failure is explained (a failed start carries the journal
	// line that says why), so it stays until the operator navigates away.
	document.addEventListener("DOMContentLoaded", function () {
		var f = document.getElementById("flash");
		if (!f || f.classList.contains("error-box")) return;
		setTimeout(function () {
			f.style.transition = "opacity .5s ease";
			f.style.opacity = "0";
			setTimeout(function () { f.remove(); }, 600);
		}, 4500);
	});

	// loadout pickers: live highlight on radio change
	document.addEventListener("change", function (e) {
		var row = e.target.closest(".lo-row");
		if (!row) return;
		paintRow(row);
	});

	function paintRow(row) {
		for (var c of row.querySelectorAll(".item-card")) {
			var input = c.querySelector("input");
			c.classList.toggle("selected", !!input && input.checked);
		}
	}

	// "Reset to defaults" selects the default tile in every picker — the button
	// existed with no handler at all, so clicking it silently did nothing. It
	// only changes the form; the operator still has to save.
	document.addEventListener("click", function (e) {
		var btn = e.target.closest('[data-act="reset-loadout"]');
		if (!btn) return;
		e.preventDefault();
		for (var row of document.querySelectorAll(".lo-row")) {
			var picked = null;
			for (var input of row.querySelectorAll("input[type=radio]")) {
				// "" is the default glove/agent, "default" the default knife.
				if (input.value === "" || input.value === "default") {
					picked = input;
					break;
				}
			}
			// Catalogs always ship a default first; fall back to it positionally.
			if (!picked) picked = row.querySelector("input[type=radio]");
			if (picked) picked.checked = true;
			paintRow(row);
		}
		cs2aToast("Everything set back to default — press Save loadout to apply.", "ok");
	});
})();
