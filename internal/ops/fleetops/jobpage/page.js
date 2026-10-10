(function () {
  "use strict";
  var charts = document.querySelectorAll("svg[data-chart]");
  var steps = document.querySelectorAll("button[data-from]");
  var whole = document.getElementById("whole");
  var label = document.getElementById("focus");
  if (!whole || !label || charts.length === 0) {
    return;
  }
  function pad(n) {
    return n < 10 ? "0" + n : String(n);
  }
  function offset(ms) {
    var s = Math.round(ms / 1000);
    var h = Math.floor(s / 3600);
    var m = Math.floor(s / 60) % 60;
    s %= 60;
    return "+" + (h > 0 ? h + "h" + pad(m) + "m" : m + "m") + pad(s) + "s";
  }
  function focus(from, to, text, step) {
    for (var i = 0; i < charts.length; i++) {
      var svg = charts[i];
      svg.setAttribute("viewBox", from + " 0 " + Math.max(to - from, 1) + " 100");
      svg.parentNode.querySelector(".x0").textContent = offset(from);
      svg.parentNode.querySelector(".x1").textContent = offset(to);
      var bands = svg.querySelectorAll("rect[data-step]");
      for (var j = 0; j < bands.length; j++) {
        bands[j].classList.toggle("on", bands[j].getAttribute("data-step") === step);
      }
    }
    for (var k = 0; k < steps.length; k++) {
      steps[k].setAttribute("aria-pressed", String(steps[k].getAttribute("data-step") === step));
    }
    label.textContent = text;
  }
  function choose(event) {
    var b = event.currentTarget;
    var step = b.getAttribute("data-step");
    focus(Number(b.getAttribute("data-from")), Number(b.getAttribute("data-to")), "Showing step " + step + ".", step);
  }
  for (var i = 0; i < steps.length; i++) {
    steps[i].addEventListener("click", choose);
  }
  whole.addEventListener("click", function () {
    focus(Number(whole.getAttribute("data-from")), Number(whole.getAttribute("data-to")), "Showing the whole series.", null);
  });
})();
