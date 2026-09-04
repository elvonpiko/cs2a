// cs2a panel glue: styled confirm modal, toasts, htmx niceties.
(function () {
	"use strict";

	// --- styled confirm modal (replaces window.confirm) --------------------
	// askConfirm(message, {danger}) -> Promise<boolean>
	function askConfirm(message, opts) {
		opts = opts || {};
		return new Promise(function (resolve) {
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
				resolve(val);
			}
			function onKey(e) {
				if (e.key === "Escape") done(false);
				if (e.key === "Enter") done(true);
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

	// intercept every data-confirm action (submit buttons + plain buttons)
	document.addEventListener("click", function (ev) {
		var btn = ev.target.closest("[data-confirm]");
		if (!btn) return;
		ev.preventDefault();
		ev.stopPropagation();
		var msg = btn.getAttribute("data-confirm");
		var danger = btn.classList.contains("danger") || btn.hasAttribute("data-confirm-danger");
		var submitForm = btn.closest("form");
		askConfirm(msg, {
			danger: danger,
			okLabel: danger ? "Yes, do it" : "Confirm",
			title: danger ? "This will affect the live server" : "Confirm action"
		}).then(function (yes) {
			if (!yes) return;
			btn.removeAttribute("data-confirm");
			if (submitForm && submitForm.requestSubmit) submitForm.requestSubmit(btn);
			else if (submitForm) submitForm.submit();
			else btn.click();
		});
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

	// auto-dismiss flash notices
	document.addEventListener("DOMContentLoaded", function () {
		setTimeout(function () {
			var f = document.getElementById("flash");
			if (f) {
				f.style.transition = "opacity .5s ease";
				f.style.opacity = "0";
				setTimeout(function () { f.remove(); }, 600);
			}
		}, 4500);
	});

	// loadout pickers: live highlight on radio change
	document.addEventListener("change", function (e) {
		var row = e.target.closest(".lo-row");
		if (!row) return;
		for (var c of row.querySelectorAll(".item-card")) {
			c.classList.toggle("selected", c.querySelector("input").checked);
		}
	});
})();
