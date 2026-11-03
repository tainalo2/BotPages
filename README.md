# BotPages

Service Go léger de publication de pages statiques pour les bots, avec panel React et endpoint MCP. Les publications modifient les fichiers sur le volume sans reconstruire l’image.

## Fonctions vérifiées

- Pages publiques non répertoriées (`X-Robots-Tag: noindex, nofollow`, slug aléatoire). Une URL partagée reste accessible publiquement.
- Pages protégées par HTTP Basic (comptes lecteurs configurés côté serveur), y compris fichiers médias et requêtes Range.
- Publication HTML/CSS/JS par API JSON, fichiers binaires par API PUT (32 MiB maximum), sites et fichiers listables dans le panel.
- MCP Streamable HTTP (`POST /mcp`) : `initialize`, `tools/list`, `tools/call` (`publish_page`, `list_sites`). Les assets volumineux passent par l’API HTTP.
- Jeton par bot limité au site de même nom ; création de site protégé réservée à l’administrateur.

## Configuration

Variables obligatoires : `BOTPAGES_ADMIN_TOKEN`, `BOTPAGES_DATA`. Variables optionnelles : `BOTPAGES_BOT_TOKENS` (objet JSON `{"newser":"..."}`), `BOTPAGES_READERS` (objet JSON par site `{"private":{"utilisateur":"mot-de-passe"}}`), `BOTPAGES_ADMIN_HOST` et `BOTPAGES_CONTENT_HOST` (obligatoirement ensemble ; identiques pour le mode domaine fixe), `BOTPAGES_ADMIN_DIR`, `BOTPAGES_ADDR`. Aucun secret n’est inclus dans le dépôt ou l’image. Le port par défaut est 8080, les données persistent dans `/data`.

L’administration se trouve sur `/admin/`. Le jeton saisi par l’utilisateur reste en mémoire dans l’onglet, non stocké par le navigateur. Pour publier : `POST /api/publish` avec un JSON `{ "site":"newser", "path":"index.html", "content":"<h1>...", "visibility":"unlisted" }` et `Authorization: Bearer <jeton>`. Les binaires utilisent `PUT /api/assets/<site>/<chemin>` avec le corps brut.

**Domaine fixe (MyInfra)** : configurer les deux variables d’hôte à `botpages.alexandre-rongier.fr`. Toutes les URL de publication deviennent `https://botpages.alexandre-rongier.fr/p/<slug>/<chemin>` ; `/admin/`, `/api/` et `/mcp` restent sur le même hôte. MyInfra route déjà cet hôte et son certificat TLS ; aucun joker DNS n’est requis. Le proxy doit interdire l’accès direct au port 8080. `/health` est une sonde sans secret utilisable avec tout Host.

**Isolation des pages** : chaque réponse `/p/` porte `Content-Security-Policy: sandbox allow-scripts; frame-ancestors 'none'` ; un document HTML, y compris ouvert directement, s’exécute avec une origine opaque (sans `allow-same-origin`). Cela interdit aux scripts des pages l’accès aux cookies, au stockage, à l’API d’admin et au contenu des autres sites via `fetch`. Les formats actifs SVG, XHTML, XML et PDF sont refusés en publication et en lecture. HTML, CSS, scripts classiques, images raster et vidéo sont pris en charge. Les scripts modules et `fetch` depuis les pages sont bloqués par CORS dans ce mode : **ne pas retirer la sandbox ni ajouter `Access-Control-Allow-Origin: null` pour contourner cela**, car toutes les pages opaques pourraient alors accéder aux ressources des autres sites. Les comptes lecteurs Basic sont distincts par site et le serveur vérifie chaque asset et requête Range. Les liens vers les ressources restent sous `/p/<slug>/...`.

Le mode ancien à origines distinctes reste possible avec deux hôtes différents et un routage `https://<slug>.<BOTPAGES_CONTENT_HOST>` ; il exige un wildcard que MyInfra n’accepte pas. Les réponses de contenu portent `Cache-Control: private, no-store`, `Origin-Agent-Cluster: ?1` et ne fournissent pas de CORS. Le partage de mots de passe entre sites est déconseillé. Les écritures API/MCP sont limitées à 60 requêtes/minute par jeton et instance, à 32 MiB par asset et à 512 MiB par site (versions précédentes incluses). La dernière version d’un fichier est sauvegardée localement lors d’un remplacement ; `POST /api/rollback` avec Bearer et `{"site":"newser","path":"index.html"}` restaure cette version. Faire aussi des sauvegardes du volume : un rollback local n’est pas une sauvegarde après perte du volume.

## Build et test

`docker build -t botpages:0.1.0 .` (exécute aussi `go test ./...` et `vite build`). L’image est construite avec Go 1.24, Node 22 et Alpine ; elle s’exécute sans root. Prévoir un volume persistant sur `/data` et HTTPS. Le mode domaine fixe exige la CSP sandbox ; un test navigateur a confirmé l’origine opaque, CSS/JS classique/image et le blocage de `fetch`, des modules et de la lecture inter-sites.

## Limites et conditions de déploiement

Pas d’antivirus, upload reprenable, gestion de lecteurs dans le panel ou OIDC. Les comptes Basic par site sont configurés dans une variable sécurisée et ne doivent être utilisés qu’en HTTPS. Les fichiers HTML sont servis tels quels, non assainis, mais leur réponse est sandboxée ; cette isolation repose sur l’application effective de la CSP par le navigateur. En configuration locale sans hôtes, l’isolation des origines est désactivée : ne pas exposer cette configuration. La limite par jeton est en mémoire, par réplica, et ne remplace pas une protection de bordure contre les attaques anonymes. Les quotas sont applicatifs et requièrent un volume privé ; ne pas monter un volume modifiable par d’autres acteurs. Le déploiement du mode fixe requiert que les deux variables d’hôte soient identiques ; Infraer doit modifier le paquet MyInfra puis vérifier une publication de bout en bout sur le domaine réel.

La génération du paquet MyInfra et les changements DNS/déploiement sont à coordonner avec Infraer ; ce dépôt n’altère pas MyInfra.
