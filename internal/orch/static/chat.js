(function () {
  var form = document.getElementById("composer");
  var box = document.getElementById("transcript");
  var peers = document.getElementById("peers");
  var phase = document.getElementById("phase");
  if (!box) return;
  var path = location.pathname.replace(/\/+$/, "");
  var es = new EventSource(path + "/events");
  es.onmessage = function (ev) {
    var data;
    try {
      data = JSON.parse(ev.data);
    } catch (e) {
      return;
    }
    if (phase && data.phase) {
      phase.textContent = data.phase;
      if (data.phaseClass) phase.className = data.phaseClass;
    }
    if (data.transcript != null && box.innerHTML !== data.transcript) {
      box.innerHTML = data.transcript;
      box.scrollTop = box.scrollHeight;
    }
    if (peers && data.peers != null && peers.innerHTML !== data.peers) {
      peers.innerHTML = data.peers;
    }
  };
  if (!form || !window.fetch) return;
  form.addEventListener("submit", function (ev) {
    ev.preventDefault();
    var field = form.querySelector("textarea");
    fetch(form.action, { method: "POST", body: new FormData(form), credentials: "same-origin" })
      .then(function (res) {
        if (res.ok && field) field.value = "";
      })
      .catch(function () {
        form.submit();
      });
  });
})();
