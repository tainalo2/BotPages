# Journal des versions

Format : [SemVer](https://semver.org/lang/fr/). Chaque version est publiée par le workflow `Release` (image `ghcr.io/tainalo2/botpages:X.Y.Z`, release GitHub avec digest).

## 0.2.1
- Journal des versions et procédure de mise à jour documentés. Version de contrôle du mécanisme de mise à jour MyInfra (remplacement sur place, volume conservé).

## 0.2.0
- Jeton **publisher** partagé (`BOTPAGES_PUBLISHER_TOKENS`) : publication sur tout site non répertorié, aucun accès aux sites protégés ni à l'administration.
- Journal d'audit (`audit action=… actor=… bot=…`) avec l'en-tête déclaratif `X-BotPages-Bot`.
- Version exposée par `/health` et le `serverInfo` MCP ; images versionnées `X.Y.Z` (+ `latest`), `edge` pour `main`.

## 0.1.0 (non versionnée)
- Service initial : publication HTML/CSS/JS/médias, pages non répertoriées et protégées, panel, MCP, domaine fixe avec sandbox CSP.
