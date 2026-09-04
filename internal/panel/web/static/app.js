// cs2a panel glue: confirm dialogs, toasts, htmx niceties.
(function () {
	"use strict";

	// --- data-confirm on any submit button -------------------------------
	document.addEventListener("click", function (ev) {
		var btn = ev.target.closest("button[data-confirm]");
		if (!btn) return;
		var form = btn.closest("form");
		if (!form) return;
		ev.preventDefault();
		if (window.confirm(btn.getAttribute("data-confirm"))) {
			// re-submit without the handler so we don't loop
			btn.removeAttribute("data-confirm");
			if (form.requestSubmit) form.requestSubmit(btn);
			else form.submit();
		}
	});

	// --- toast helper -----------------------------------------------------
	var toastTimer = null;
	function toast(msg, kind) {
		var el = document.getElementById("toast");
		if (!el) return;
		el.textContent = msg;
		el.className = "show " + (kind || "");
		clearTimeout(toastTimer);
		toastTimer = setTimeout(function () { el.className = ""; }, 3200);
	}
	window.cs2aToast = toast;

	// surface flash messages + htmx errors as toasts
	document.addEventListener("DOMContentLoaded", function () {
		var flash = document.getElementById("flash");
		if (flash) toast(flash.textContent.trim(), flash.classList.contains("error-box") ? "err" : "ok");
	});
	document.body.addEventListener("htmx:responseError", function () {
		toast("Something went wrong — check the server logs.", "err");
	});
	document.body.addEventListener("htmx:sendError", function () {
		toast("Lost connection to the panel.", "err");
	});

	// --- auto-dismiss notices ---------------------------------------------
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
})();
