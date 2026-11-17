// The console script is served from the binary; the CSP disallows inline scripts.
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
      var isHistory = button.textContent.indexOf("歷史") !== -1 || button.getAttribute("aria-controls") === "task-list-done";
      button.textContent = expanded ? (isHistory ? "收起歷史" : "收起其餘任務") : (isHistory ? "展開其餘歷史" : "展開其餘任務");
    });
  });
  document.querySelectorAll("[data-rollout-selection]").forEach(function (form) {
    var summary = form.querySelector("[data-rollout-summary]");
    var update = function () {
      var n = form.querySelectorAll('input[name="device"]:checked').length;
      if (summary) summary.textContent = n ? ("已選擇 " + n + " 台機器；提交後這些機器將不再跟隨全局版本") : "尚未選擇機器";
    };
    form.addEventListener("change", update);
    update();
  });
  var ticketForm = document.querySelector("[data-ticket-form]");
  if (ticketForm) {
    var ticketType = ticketForm.querySelector('[name="type"]');
    var ticketSubtype = ticketForm.querySelector('[name="subtype"]');
    var blockWebsites = ticketForm.querySelector('[name="block_websites"]');
    var blockLLM = ticketForm.querySelector('[name="block_llm"]');
    var changeType = ticketForm.querySelector('[name="change_type"]');
    var changeUrgent = ticketForm.querySelector('[name="change_urgent"]');
    var updateTicketFields = function () {
      ticketForm.querySelectorAll("[data-ticket-fields]").forEach(function (field) {
        var visible = field.dataset.ticketFields.split(" ").includes(ticketType.value);
        field.hidden = !visible;
      });
      ticketForm.querySelectorAll("[data-ticket-subtype]").forEach(function (field) {
        field.hidden = ticketType.value !== "general" || field.dataset.ticketSubtype !== ticketSubtype.value;
      });
      ticketForm.querySelectorAll("[data-ticket-block]").forEach(function (field) {
        field.hidden = ticketSubtype.value !== "website" || blockWebsites.value !== field.dataset.ticketBlock;
      });
      ticketForm.querySelectorAll("[data-ticket-llm]").forEach(function (field) {
        field.hidden = ticketSubtype.value !== "website" || blockWebsites.value !== "yes" || blockLLM.value !== field.dataset.ticketLlm;
      });
      ticketForm.querySelectorAll("[data-ticket-change-type]").forEach(function (field) {
        field.hidden = ticketType.value !== "general" || ticketSubtype.value !== "environment" || changeType.value !== field.dataset.ticketChangeType;
      });
      ticketForm.querySelectorAll("[data-ticket-urgent]").forEach(function (field) {
        field.hidden = ticketType.value !== "general" || ticketSubtype.value !== "environment" || changeUrgent.value !== field.dataset.ticketUrgent;
      });
      var priority = ticketForm.querySelector("[data-ticket-priority]");
      if (priority) { priority.hidden = ticketType.value === "general" && ticketSubtype.value === "environment"; }
      ticketForm.querySelectorAll("input, select, textarea").forEach(function (control) {
        control.disabled = !!control.closest("[hidden]");
      });
    };
    ticketType.addEventListener("change", updateTicketFields);
    ticketSubtype.addEventListener("change", updateTicketFields);
    blockWebsites.addEventListener("change", updateTicketFields);
    blockLLM.addEventListener("change", updateTicketFields);
    changeType.addEventListener("change", updateTicketFields);
    changeUrgent.addEventListener("change", updateTicketFields);
    updateTicketFields();
  }
})();
