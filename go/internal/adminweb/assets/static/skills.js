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
      count.textContent = "已选 " + selected + " 项";
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
})();
