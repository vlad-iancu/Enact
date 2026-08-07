package enactmain

import (
	"html/template"
	"net/http"
)

// Minimal built-in pages so the auth flows are usable end to end before a
// real frontend exists. No external assets.

var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>enact — login</title><style>
body{font-family:system-ui;max-width:24rem;margin:4rem auto;padding:0 1rem}
input,button{width:100%;padding:.5rem;margin:.25rem 0;box-sizing:border-box}
fieldset{border:1px solid #ccc;margin:1rem 0}.err{color:#b00020;min-height:1.2em}
a.google{display:block;text-align:center;padding:.6rem;border:1px solid #444;border-radius:4px;text-decoration:none;color:inherit}
</style></head><body>
<h1>enact</h1>
<a class="google" href="/auth/google">Login with Google</a>
<fieldset><legend>Log in</legend>
<input id="li-email" type="email" placeholder="email">
<input id="li-pass" type="password" placeholder="password">
<button onclick="post('/auth/login',{email:v('li-email'),password:v('li-pass')})">Log in</button>
</fieldset>
<fieldset><legend>Register</legend>
<input id="rg-name" placeholder="display name">
<input id="rg-email" type="email" placeholder="email">
<input id="rg-pass" type="password" placeholder="password (min 8 chars)">
<button onclick="post('/auth/register',{display_name:v('rg-name'),email:v('rg-email'),password:v('rg-pass')})">Register</button>
</fieldset>
<div class="err" id="err"></div>
<script>
function v(id){return document.getElementById(id).value}
async function post(url,body){
  const r=await fetch(url,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
  if(r.ok){location.href='/app';return}
  const d=await r.json().catch(()=>({}));
  document.getElementById('err').textContent=d.error||('HTTP '+r.status);
}
</script></body></html>`))

var appTmpl = template.Must(template.New("app").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>enact — home</title><style>
body{font-family:system-ui;max-width:40rem;margin:4rem auto;padding:0 1rem}
button{padding:.4rem .8rem}
</style></head><body>
<h1>Welcome, {{.DisplayName}}</h1>
<p>Logged in as <b>{{.Email}}</b>.</p>
<p>This is the enact platform homepage placeholder.</p>
<button onclick="fetch('/auth/logout',{method:'POST'}).then(()=>location.href='/login')">Log out</button>
</body></html>`))

func renderLoginPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = loginTmpl.Execute(w, nil)
}

func renderAppPage(w http.ResponseWriter, sess Session) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = appTmpl.Execute(w, sess)
}
