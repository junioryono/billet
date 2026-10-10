(function () {
  "use strict";
  var charts = document.querySelectorAll("svg[data-chart]");
  var steps = document.querySelectorAll("button[data-key]");
  var whole = document.getElementById("whole");
  var label = document.getElementById("focus");
  if (!whole || !label || charts.length === 0) {
    return;
  }
  var origin = Number(whole.getAttribute("data-origin"));
  var scale = Number(whole.getAttribute("data-width")) / Number(whole.getAttribute("data-to"));
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
  function focus(from, to, text, key) {
    for (var i = 0; i < charts.length; i++) {
      var svg = charts[i];
      svg.setAttribute("viewBox", from + " 0 " + Math.max(to - from, 1) + " 100");
      svg.parentNode.querySelector(".x0").textContent = offset(origin + from * scale);
      svg.parentNode.querySelector(".x1").textContent = offset(origin + to * scale);
      var bands = svg.querySelectorAll("rect[data-key]");
      for (var j = 0; j < bands.length; j++) {
        bands[j].classList.toggle("on", bands[j].getAttribute("data-key") === key);
      }
    }
    for (var k = 0; k < steps.length; k++) {
      steps[k].setAttribute("aria-pressed", String(steps[k].getAttribute("data-key") === key));
    }
    label.textContent = text;
  }
  function choose(event) {
    var b = event.currentTarget;
    focus(Number(b.getAttribute("data-from")), Number(b.getAttribute("data-to")),
      "Showing step " + b.getAttribute("data-step") + ".", b.getAttribute("data-key"));
  }
  for (var i = 0; i < steps.length; i++) {
    steps[i].addEventListener("click", choose);
  }
  whole.addEventListener("click", function () {
    focus(Number(whole.getAttribute("data-from")), Number(whole.getAttribute("data-to")), "Showing the whole series.", null);
  });
})();
