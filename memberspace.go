package main

// memberSpaceEntryJS est le entry.js de la mini-app "espace membre" utilisée
// par les tests d'intégration serveur. Le fichier publiable final (packagé
// pour /api/apps) vit sous member-space-app/entry.js et doit rester
// identique à cette constante — voir TestMemberSpaceAppFilesMatchEmbeddedEntry.
const memberSpaceEntryJS = `
function jsonResponse(status, data) {
  return {
    status: status,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(data),
  };
}

function handle(request) {
  if (request.method === "POST" && request.path === "/register") {
    var input = JSON.parse(request.body || "{}");
    members.register(input.username, input.password);
    return jsonResponse(200, { ok: true });
  }

  if (request.method === "POST" && request.path === "/login") {
    var input = JSON.parse(request.body || "{}");
    var result = members.login(input.username, input.password);
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
    return jsonResponse(200, { user: user, message: "Connecté en tant que " + user.username });
  }

  return jsonResponse(404, { error: "not found" });
}
`
