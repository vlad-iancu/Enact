package service

import (
	"html/template"
	"net/http"

	restful "github.com/emicklei/go-restful/v3"
)

// homeWebService serves a small HTML page at GET / with two tabs: one
// fetching /healthz, the other embedding the in-binary Swagger UI via an
// <iframe>. The page itself loads no external assets, so the service is
// fully usable offline. The supplied name is used as both the document
// title and the visible header.
func homeWebService(name string) *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/")
	ws.Route(ws.GET("").
		To(makeHomeHandler(name)).
		Doc("Service home page (health + swagger UI)").
		Produces("text/html").
		Returns(http.StatusOK, "OK", nil))
	return ws
}

var homeTmpl = template.Must(template.New("home").Parse(homeHTML))

func makeHomeHandler(name string) restful.RouteFunction {
	return func(_ *restful.Request, resp *restful.Response) {
		resp.Header().Set("Content-Type", "text/html; charset=utf-8")
		resp.WriteHeader(http.StatusOK)
		_ = homeTmpl.Execute(resp, map[string]string{"Name": name})
	}
}

const homeHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>{{.Name}}</title>
<style>
  html, body { margin: 0; height: 100%; }
  body { display: flex; flex-direction: column; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; color: #222; }
  header { padding: 14px 24px; border-bottom: 1px solid #e5e5e5; background: #fafafa; }
  header h1 { margin: 0; font-size: 18px; font-weight: 600; }
  .tabs { display: flex; border-bottom: 1px solid #e5e5e5; background: #fff; }
  .tab { padding: 12px 24px; cursor: pointer; border: none; background: none; font-size: 14px; color: #555; border-bottom: 2px solid transparent; }
  .tab:hover { color: #1976d2; }
  .tab.active { color: #1976d2; border-bottom-color: #1976d2; font-weight: 600; }
  .panels { flex: 1; display: flex; min-height: 0; }
  .panel { display: none; flex: 1; min-height: 0; overflow: auto; }
  .panel.active { display: flex; flex-direction: column; }
  #health-panel { padding: 24px; }
  #health-panel pre { background: #f5f5f5; padding: 12px; border-radius: 4px; font-size: 13px; margin: 0; }
  #health-panel .meta { color: #777; font-size: 12px; margin: 0 0 12px; }
  #swagger-panel { padding: 0; }
  #swagger-frame { width: 100%; height: 100%; border: 0; flex: 1; }
</style>
</head>
<body>
<header><h1>{{.Name}}</h1></header>
<div class="tabs">
  <button class="tab active" data-panel="health-panel">Health</button>
  <button class="tab" data-panel="swagger-panel">Swagger UI</button>
</div>
<div class="panels">
  <div id="health-panel" class="panel active">
    <p class="meta">Polling <code>GET /healthz</code> every 5 seconds.</p>
    <pre id="health-output">Loading…</pre>
  </div>
  <div id="swagger-panel" class="panel">
    <iframe id="swagger-frame" title="Swagger UI"></iframe>
  </div>
</div>
<script>
(function () {
  var tabs = document.querySelectorAll('.tab');
  var panels = document.querySelectorAll('.panel');
  var swaggerLoaded = false;
  var frame = document.getElementById('swagger-frame');

  tabs.forEach(function (t) {
    t.addEventListener('click', function () {
      tabs.forEach(function (x) { x.classList.remove('active'); });
      panels.forEach(function (p) { p.classList.remove('active'); });
      t.classList.add('active');
      document.getElementById(t.dataset.panel).classList.add('active');
      if (t.dataset.panel === 'swagger-panel' && !swaggerLoaded) {
        frame.src = '/swagger-ui/';
        swaggerLoaded = true;
      }
    });
  });

  var out = document.getElementById('health-output');
  function refreshHealth() {
    fetch('/healthz', { cache: 'no-store' })
      .then(function (r) { return r.json().then(function (j) { return { ok: r.ok, body: j }; }); })
      .then(function (res) {
        out.textContent = (res.ok ? '' : 'HTTP error\n') + JSON.stringify(res.body, null, 2);
      })
      .catch(function (e) { out.textContent = 'Error: ' + e; });
  }
  refreshHealth();
  setInterval(refreshHealth, 5000);
})();
</script>
</body>
</html>
`
