// The console's only script. It is served from the binary, under a CSP that
// allows nothing inline, and every page works without it: the server treats
// the "all" box as the decision whatever the other boxes say. This just keeps
// the form from showing two contradictory answers at once.
(function () {
  "use strict";
  document.querySelectorAll("fieldset.checks").forEach(function (set) {
    var all = set.querySelector('input[name="all"]');
    if (!all) { return; }
    var models = set.querySelectorAll('input[name="models"]');
    function apply() {
      models.forEach(function (m) {
        if (all.checked) { m.checked = false; }
        m.disabled = all.checked;
      });
    }
    all.addEventListener("change", apply);
    models.forEach(function (m) {
      m.addEventListener("change", function () {
        if (m.checked && all.checked) { all.checked = false; apply(); }
      });
    });
    apply();
  });
  document.querySelectorAll("form[data-confirm]").forEach(function (form) {
    form.addEventListener("submit", function (event) {
      if (!window.confirm(form.getAttribute("data-confirm"))) {
        event.preventDefault();
      }
    });
  });
})();
