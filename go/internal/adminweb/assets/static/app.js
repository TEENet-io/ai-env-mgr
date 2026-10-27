// The console's only script. It is served from the binary under a CSP that
// allows nothing inline. Most forms work without it; the channel trial uses
// fetch so prompts and replies remain on the page instead of entering a URL.
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
  document.querySelectorAll("form[data-channel-trial]").forEach(function (form) {
    var result = form.parentElement.querySelector("[data-channel-result]");
    if (!result) { return; }
    var state = result.querySelector("[data-channel-state]");
    var transcript = result.querySelector("[data-channel-transcript]");
    var input = result.querySelector("[data-channel-input]");
    var output = result.querySelector("[data-channel-output]");
    var meta = result.querySelector("[data-channel-meta]");
    var button = form.querySelector('button[type="submit"]');
    form.addEventListener("submit", async function (event) {
      event.preventDefault();
      if (!form.reportValidity() || button.disabled) { return; }
      var entered = form.querySelector('[name="prompt"]').value.trim();
      result.hidden = false;
      transcript.hidden = true;
      result.classList.remove("is-error");
      state.textContent = "正在向指定部署发送测试请求…";
      meta.textContent = "";
      button.disabled = true;
      try {
        var sendTrial = function () {
          return fetch(form.action, {
            method: "POST", body: new FormData(form), credentials: "same-origin",
            headers: { "Accept": "application/json" }, cache: "no-store"
          });
        };
        var response = await sendTrial();
        var data = await response.json();
        if (response.status === 403 && data.error === "表单已过期，请刷新页面重试") {
          var fresh = await fetch("/channels", { credentials: "same-origin", cache: "no-store" });
          if (fresh.ok && new URL(fresh.url).pathname === "/channels") {
            var page = new DOMParser().parseFromString(await fresh.text(), "text/html");
            var token = page.querySelector('form[data-channel-trial] input[name="csrf"]');
            if (token && token.value) {
              form.querySelector('input[name="csrf"]').value = token.value;
              response = await sendTrial();
              data = await response.json();
            }
          }
        }
        var passed = response.ok && data.ok;
        result.classList.toggle("is-error", !passed);
        state.textContent = passed ? "试调用成功" : (data.error || "试调用失败");
        input.textContent = data.input || entered;
        output.textContent = passed ? (data.output || "模型未返回可显示文本") : "没有模型回复";
        transcript.hidden = false;
        var details = [];
        if (data.model_name) { details.push(data.model_name); }
        if (data.model_id) { details.push("部署 " + data.model_id); }
        if (data.elapsed_ms !== undefined) { details.push((data.elapsed_ms / 1000).toFixed(1) + " 秒"); }
        if (passed || data.completion_tokens !== undefined) { details.push("输入 " + (data.prompt_tokens || 0) + " tokens · 输出 " + (data.completion_tokens || 0) + " tokens"); }
        if (data.finish_reason) { details.push("结束原因 " + data.finish_reason); }
        if (data.warning) { details.push(data.warning); }
        meta.textContent = details.join(" · ");
      } catch (_) {
        result.classList.add("is-error");
        state.textContent = "无法读取测试结果，请检查登录状态和网络后重试";
        input.textContent = entered;
        output.textContent = "没有模型回复";
        transcript.hidden = false;
        meta.textContent = "";
      } finally {
        button.disabled = false;
      }
    });
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
