(function () {
  "use strict";
  document.querySelectorAll("[data-skill-picker]").forEach(function (picker) {
    var filter = picker.querySelector("[data-skill-filter]");
    var choices = Array.from(picker.querySelectorAll("[data-skill-choice]"));
    var count = picker.querySelector("[data-skill-picked]");
    var empty = picker.querySelector("[data-skill-no-results]");
    var tools = picker.querySelector("[data-skill-picker-tools]");
    if (!filter || !tools) return;
    tools.hidden = false;
    function update() {
      var query = filter.value.trim().toLocaleLowerCase();
      var visible = 0, selected = 0;
      choices.forEach(function (choice) {
        // Filtering must never disable or clear a previously selected person.
        choice.hidden = choice.textContent.toLocaleLowerCase().indexOf(query) === -1;
        if (!choice.hidden) visible++;
        if (choice.querySelector("input").checked) selected++;
      });
      count.textContent = "已選 " + selected + " 項";
      empty.hidden = choices.length === 0 || visible > 0;
    }
    filter.addEventListener("input", update);
    filter.addEventListener("keydown", function (event) {
      if (event.key === "Enter") event.preventDefault();
    });
    picker.addEventListener("change", update);
    picker.querySelector("[data-skill-select]").addEventListener("click", function () {
      choices.forEach(function (choice) {
        if (!choice.hidden) choice.querySelector("input").checked = true;
      });
      update();
    });
    picker.querySelector("[data-skill-clear]").addEventListener("click", function () {
      choices.forEach(function (choice) { choice.querySelector("input").checked = false; });
      update();
    });
    update();
  });

  // The dashed upload boxes look like drop zones, so make them behave like
  // ones: a file dropped anywhere on the box goes to its file input.
  var zones = Array.from(document.querySelectorAll(".skill-upload, .skill-attachment-upload"));
  function accepts(input, file) {
    var accept = (input.getAttribute("accept") || "").toLowerCase().split(",").map(function (s) { return s.trim(); }).filter(Boolean);
    if (accept.length === 0) return true;
    var name = file.name.toLowerCase(), type = (file.type || "").toLowerCase();
    return accept.some(function (rule) {
      return rule.charAt(0) === "." ? name.slice(-rule.length) === rule : rule === type;
    });
  }
  zones.forEach(function (zone) {
    var input = zone.querySelector("input[type=file]");
    if (!input || typeof DataTransfer === "undefined") return;
    var depth = 0;
    zone.addEventListener("dragenter", function (event) {
      event.preventDefault();
      depth++;
      zone.classList.add("is-dragover");
    });
    zone.addEventListener("dragover", function (event) {
      event.preventDefault();
      event.dataTransfer.dropEffect = "copy";
    });
    zone.addEventListener("dragleave", function () {
      depth = Math.max(0, depth - 1);
      if (depth === 0) zone.classList.remove("is-dragover");
    });
    zone.addEventListener("drop", function (event) {
      event.preventDefault();
      depth = 0;
      zone.classList.remove("is-dragover");
      var dropped = Array.from(event.dataTransfer.files || []);
      if (!input.multiple) dropped = dropped.slice(0, 1);
      var kept = dropped.filter(function (file) { return accepts(input, file); });
      if (kept.length === 0) {
        if (dropped.length > 0) window.alert(input.multiple ? "這些文件類型不支持上傳。" : "請拖入 .zip 格式的 Skill 包。");
        return;
      }
      if (kept.length < dropped.length) window.alert("已略過不支持的文件類型。");
      var transfer = new DataTransfer();
      kept.forEach(function (file) { transfer.items.add(file); });
      input.files = transfer.files;
      input.dispatchEvent(new Event("change", { bubbles: true }));
    });
  });
  // A file dropped beside a box must not make the browser open or download it
  // and lose the form.
  if (zones.length > 0) {
    ["dragover", "drop"].forEach(function (type) {
      window.addEventListener(type, function (event) {
        if (event.dataTransfer && Array.from(event.dataTransfer.types || []).indexOf("Files") !== -1) event.preventDefault();
      });
    });
  }
})();
