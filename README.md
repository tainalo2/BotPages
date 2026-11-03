# BotPages

Service Go léger de publication de pages statiques pour les bots, avec panel React et endpoint MCP. Les publications modifient les fichiers sur le volume sans reconstruire l’image.

## Fonctions vérifiées

- Pages publiques non répertoriées (`X-Robots-Tag: noindex, nofollow`, slug aléatoire). Une URL partagée reste accessible publiquement.
- Pages protégées par HTTP Basic (comptes lecteurs configurés côté serveur), y compris fichiers médias et requêtes Range.
- Publication HTML/CSS/JS par API JSON, fichiers binaires par API PUT (256 MiB maximum), sites et fichiers listables dans le panel.
- MCP Streamable HTTP (`POST /mcp`) : `initialize`, `tools/list`, `tools/call` (`publish_page`, `list_sites`). Les assets volumineux passent par l’API HTTP.
- Jeton par bot limité au site de même nom ; création de site protégé réservée à l’administrateur.

## Configuration

Variables obligatoires : `BOTPAGES_ADMIN_TOKEN`, `BOTPAGES_DATA`. Variables optionnelles : `BOTPAGES_BOT_TOKENS` (objet JSON `{"newser":"..."}`), `BOTPAGES_READERS` (objet JSON par site `{"private":{"utilisateur":"mot-de-passe"}}`), `BOTPAGES_ADMIN_HOST` et `BOTPAGES_CONTENT_HOST` (obligatoirement ensemble et distincts), `BOTPAGES_ADMIN_DIR`, `BOTPAGES_ADDR`. Aucun secret n’est inclus dans le dépôt ou l’image. Le port par défaut est 8080, les données persistent dans `/data`.

L’administration se trouve sur `/admin/`. Le jeton saisi par l’utilisateur reste en mémoire dans l’onglet, non stocké par le navigateur. Pour publier : `POST /api/publish` avec un JSON `{ "site":"newser", "path":"index.html", "content":"<h1>...", "visibility":"unlisted" }` et `Authorization: Bearer <jeton>`. Les binaires utilisent `PUT /api/assets/<site>/<chemin>` avec le corps brut. Les réponses donnent l’URL absolue `https://<slug>.<BOTPAGES_CONTENT_HOST>/p/<slug>/<chemin>` en production. Le proxy doit router `*.<BOTPAGES_CONTENT_HOST>` vers ce service, avec TLS wildcard correspondant ; ne pas router l’administration vers les sous-domaines de contenu. `/health` est une sonde sans secret, utilisable avec tout Host. Ne jamais exposer directement le port 8080.

Chaque site a son propre sous-domaine et son propre jeu de comptes Basic : le cache d’authentification du navigateur est ainsi limité à son origine. Les réponses de contenu portent `Cache-Control: private, no-store`, `Origin-Agent-Cluster: ?1` et ne fournissent pas de CORS. Le partage de mots de passe entre sites est déconseillé. Les écritures API/MCP sont limitées à 60 requêtes/minute par jeton et instance, à 32 MiB par asset et à 512 MiB par site (versions précédentes incluses). La dernière version d’un fichier est sauvegardée localement lors d’un remplacement ; `POST /api/rollback` avec Bearer et `{"site":"newser","path":"index.html"}` restaure cette version. Faire aussi des sauvegardes du volume : un rollback local n’est pas une sauvegarde après perte du volume.

## Build et test

`docker build -t botpages:0.1.0 .` (exécute aussi `go test ./...` et `vite build`). L’image est construite avec Go 1.24, Node 22 et Alpine ; elle s’exécute sans root. Prévoir un volume persistant sur `/data`, HTTPS et deux origines séparées en production.

## Limites et conditions de déploiement

Pas d’antivirus, upload reprenable, gestion de lecteurs dans le panel ou OIDC. Les comptes Basic par site sont configurés dans une variable sécurisée et ne doivent être utilisés qu’en HTTPS. Les fichiers HTML sont servis tels quels, non assainis ; le sous-domaine wildcard et son certificat TLS sont une condition stricte. En configuration locale sans hôtes, l’isolation des origines est désactivée : ne pas exposer cette configuration. La limite par jeton est en mémoire, par réplica, et ne remplace pas une protection de bordure contre les attaques anonymes. Les quotas sont applicatifs et requièrent un volume privé ; ne pas monter un volume modifiable par d’autres acteurs. Vérifier les capacités wildcard du routeur MyInfra avant déploiement.

La génération du paquet MyInfra et les changements DNS/déploiement sont à coordonner avec Infraer ; ce dépôt n’altère pas MyInfra.
