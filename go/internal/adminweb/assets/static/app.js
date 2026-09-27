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
  document.querySelectorAll("[data-history-toggle]").forEach(function (button) {
    var list = document.getElementById(button.getAttribute("aria-controls"));
    if (!list) { return; }
    button.addEventListener("click", function () {
      var expanded = list.classList.toggle("is-expanded");
      button.setAttribute("aria-expanded", expanded ? "true" : "false");
      var isHistory = button.textContent.indexOf("历史") !== -1 || button.getAttribute("aria-controls") === "task-list-done";
      button.textContent = expanded ? (isHistory ? "收起历史" : "收起其余任务") : (isHistory ? "展开其余历史" : "展开其余任务");
    });
  });
  document.querySelectorAll("[data-rollout-selection]").forEach(function (form) {
    var summary = form.querySelector("[data-rollout-summary]");
    var update = function () {
      var n = form.querySelectorAll('input[name="device"]:checked').length;
      if (summary) summary.textContent = n ? ("已选择 " + n + " 台机器；提交后这些机器将不再跟随全局版本") : "尚未选择机器";
    };
    form.addEventListener("change", update);
    update();
  });
})();
