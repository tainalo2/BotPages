import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import './style.css';

function App() {
  const [token, setToken] = useState('');
  const [sites, setSites] = useState([]);
  const [tokens, setTokens] = useState([]);
  const [name, setName] = useState('');
  const [visibility, setVisibility] = useState('unlisted');
  const [html, setHtml] = useState('<!doctype html><html><body><h1>Nouvelle page</h1></body></html>');
  const [asset, setAsset] = useState(null);
  const [identityName, setIdentityName] = useState('');
  const [identityKind, setIdentityKind] = useState('bot');
  const [siteScope, setSiteScope] = useState('');
  const [createdToken, setCreatedToken] = useState('');
  const [message, setMessage] = useState('');

  const headers = () => ({ Authorization: `Bearer ${token}` });

  async function loadSites() {
    const response = await fetch('/api/sites', { headers: headers() });
    setMessage(response.ok ? '' : `Erreur ${response.status}`);
    if (response.ok) setSites(await response.json());
  }

  async function loadTokens() {
    const response = await fetch('/api/admin/tokens', { headers: headers() });
    setMessage(response.ok ? '' : `Erreur ${response.status}`);
    if (response.ok) setTokens(await response.json());
  }

  async function createIdentity(event) {
    event.preventDefault();
    setCreatedToken('');
    const body = { name: identityName, kind: identityKind };
    if (identityKind === 'bot') body.site_scope = siteScope;
    const response = await fetch('/api/admin/tokens', {
      method: 'POST',
      headers: { ...headers(), 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const text = await response.text();
    if (!response.ok) {
      setMessage(`Erreur ${response.status} : ${text}`);
      return;
    }
    const created = JSON.parse(text);
    setCreatedToken(created.token);
    setMessage('Jeton créé. Copiez-le maintenant : il ne sera plus affiché.');
    setIdentityName('');
    setSiteScope('');
    await loadTokens();
  }

  async function revokeIdentity(id) {
    const response = await fetch(`/api/admin/tokens/${id}`, { method: 'DELETE', headers: headers() });
    setMessage(response.ok ? 'Jeton révoqué.' : `Erreur ${response.status}`);
    if (response.ok) await loadTokens();
  }

  async function publish(event) {
    event.preventDefault();
    const response = await fetch('/api/publish', {
      method: 'POST',
      headers: { ...headers(), 'Content-Type': 'application/json' },
      body: JSON.stringify({ site: name, path: 'index.html', content: html, visibility }),
    });
    const text = await response.text();
    setMessage(response.ok ? `Page publiée : ${JSON.parse(text).url}` : `Erreur ${response.status} : ${text}`);
    if (response.ok) loadSites();
  }

  async function upload() {
    if (!asset) return;
    const response = await fetch(`/api/assets/${encodeURIComponent(name)}/${encodeURIComponent(asset.name)}`, {
      method: 'PUT', headers: headers(), body: asset,
    });
    setMessage(response.ok ? `Asset déposé : ${JSON.parse(await response.text()).url}` : `Erreur asset ${response.status}`);
  }

  return <main>
    <header><h1>BotPages</h1><p>Publiez des sites statiques sans redéployer le serveur.</p></header>
    <section>
      <h2>Administration</h2>
      <label>Jeton administrateur <input type="password" value={token} onChange={event => setToken(event.target.value)} autoComplete="off" /></label>
      <button onClick={loadSites}>Afficher les sites</button>
      <button onClick={loadTokens}>Afficher les jetons</button>
      <ul>{sites.map(site => <li key={site.slug}><strong>{site.name}</strong> · {site.visibility} · <code>{site.slug}</code></li>)}</ul>
    </section>
    <section>
      <h2>Jetons bots et publishers</h2>
      <form onSubmit={createIdentity}>
        <label>Nom <input required pattern="[a-z0-9-]+" value={identityName} onChange={event => setIdentityName(event.target.value)} /></label>
        <label>Type <select value={identityKind} onChange={event => setIdentityKind(event.target.value)}><option value="bot">Bot</option><option value="publisher">Publisher</option></select></label>
        {identityKind === 'bot' && <label>Site autorisé <input required pattern="[a-z0-9-]+" value={siteScope} onChange={event => setSiteScope(event.target.value)} /></label>}
        <button type="submit">Créer le jeton</button>
      </form>
      {createdToken && <div className="secret"><strong>Nouveau jeton (affiché une seule fois)</strong><code>{createdToken}</code></div>}
      <ul>{tokens.map(identity => <li key={identity.id} className={identity.revoked_at ? 'revoked' : ''}>
        <strong>{identity.name}</strong> · {identity.kind}{identity.site_scope ? ` · ${identity.site_scope}` : ''} · {identity.revoked_at ? 'révoqué' : 'actif'}
        {!identity.revoked_at && <button className="danger" onClick={() => revokeIdentity(identity.id)}>Révoquer</button>}
      </li>)}</ul>
    </section>
    <section>
      <h2>Publier une page</h2>
      <form onSubmit={publish}>
        <label>Nom du site <input required pattern="[a-z0-9-]+" value={name} onChange={event => setName(event.target.value)} /></label>
        <label>Visibilité <select value={visibility} onChange={event => setVisibility(event.target.value)}><option value="unlisted">Publique non répertoriée</option><option value="protected">Protégée</option></select></label>
        <label>index.html <textarea rows="9" value={html} onChange={event => setHtml(event.target.value)} /></label>
        <button type="submit">Publier</button>
      </form>
      <label>Asset (image, vidéo, CSS, JS) <input type="file" onChange={event => setAsset(event.target.files?.[0])}/></label>
      <button onClick={upload}>Déposer l’asset</button>
    </section>
    <p role="status">{message}</p>
  </main>;
}

createRoot(document.getElementById('root')).render(<App />);
