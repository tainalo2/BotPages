// Mini-app "espace membre" — comptes et sessions utilisateurs finaux
// (Epic 5). Les routes /register, /login, /logout et /whoami utilisent les
// API goja currentUser() et members.{register,login,logout} fournies par le
// runtime BotPages (voir memberauth.go) : aucun mot de passe en clair ni
// hash, ni jeton de session brut ne transite par ce fichier — le runtime Go
// gère la base SQLite par site, le hachage argon2id et le cookie de session
// HttpOnly/Secure/SameSite=Strict scopé au chemin de cette app.
//
// GET / rend une page HTML minimale qui pilote les mêmes routes via fetch().

function jsonResponse(status, data) {
  return {
    status: status,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(data),
  };
}

function renderPage() {
  var user = currentUser();
  var identityLine = user
    ? "Connecté en tant que " + user.username
    : "Non connecté.";

  return (
    "<!doctype html><html lang=\"fr\"><head><meta charset=\"utf-8\">" +
    "<title>Espace membre</title>" +
    "<style>body{font-family:sans-serif;max-width:28rem;margin:3rem auto}" +
    "label{display:block;margin-top:.75rem}input{width:100%;padding:.4rem}" +
    "button{margin-top:1rem;padding:.5rem 1rem;cursor:pointer}" +
    "#identity{font-weight:bold;margin-bottom:1rem}" +
    "#message{color:#a33;min-height:1.2em}</style></head><body>" +
    "<div id=\"identity\">" + identityLine + "</div>" +
    "<div id=\"message\"></div>" +
    "<form id=\"register-form\"><h2>Créer un compte</h2>" +
    "<label>Nom d'utilisateur<input name=\"username\" required></label>" +
    "<label>Mot de passe<input name=\"password\" type=\"password\" required></label>" +
    "<button type=\"submit\">S'inscrire</button></form>" +
    "<form id=\"login-form\"><h2>Se connecter</h2>" +
    "<label>Nom d'utilisateur<input name=\"username\" required></label>" +
    "<label>Mot de passe<input name=\"password\" type=\"password\" required></label>" +
    "<button type=\"submit\">Se connecter</button></form>" +
    "<button id=\"logout-btn\">Se déconnecter</button>" +
    "<script>" +
    "function showMessage(t){document.getElementById('message').textContent=t;}" +
    "function refresh(){location.reload();}" +
    "document.getElementById('register-form').addEventListener('submit', function(e){" +
    "e.preventDefault();" +
    "var f=new FormData(e.target);" +
    "fetch('register',{method:'POST',body:JSON.stringify({username:f.get('username'),password:f.get('password')})})" +
    ".then(function(r){return r.json().then(function(d){return {r:r,d:d};});})" +
    ".then(function(x){if(!x.r.ok){showMessage('Inscription refusée.');return;}showMessage('Compte créé, vous pouvez vous connecter.');});" +
    "});" +
    "document.getElementById('login-form').addEventListener('submit', function(e){" +
    "e.preventDefault();" +
    "var f=new FormData(e.target);" +
    "fetch('login',{method:'POST',body:JSON.stringify({username:f.get('username'),password:f.get('password')})})" +
    ".then(function(r){return r.json().then(function(d){return {r:r,d:d};});})" +
    ".then(function(x){if(!x.d.ok){showMessage('Identifiants invalides.');return;}refresh();});" +
    "});" +
    "document.getElementById('logout-btn').addEventListener('click', function(){" +
    "fetch('logout',{method:'POST'}).then(refresh);" +
    "});" +
    "</script></body></html>"
  );
}

function handle(request) {
  if (request.method === "GET" && request.path === "/") {
    return {
      status: 200,
      headers: { "Content-Type": "text/html; charset=utf-8" },
      body: renderPage(),
    };
  }

  if (request.method === "POST" && request.path === "/register") {
    var input = JSON.parse(request.body || "{}");
    members.register(input.username, input.password);
    return jsonResponse(200, { ok: true });
  }

  if (request.method === "POST" && request.path === "/login") {
    var loginInput = JSON.parse(request.body || "{}");
    var result = members.login(loginInput.username, loginInput.password);
    if (!result.ok) {
      return jsonResponse(401, { ok: false });
    }
    return jsonResponse(200, { ok: true, username: result.username });
  }

  if (request.method === "POST" && request.path === "/logout") {
    members.logout();
    return jsonResponse(200, { ok: true });
  }

  if (request.method === "GET" && request.path === "/whoami") {
    var user = currentUser();
    if (!user) {
      return jsonResponse(200, { user: null });
    }
    return jsonResponse(200, {
      user: user,
      message: "Connecté en tant que " + user.username,
    });
  }

  return jsonResponse(404, { error: "not found" });
}
