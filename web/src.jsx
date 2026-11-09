import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import './style.css';

function IdentityPanel({ token, setMessage }) {
  const [bots, setBots] = useState([]);
  const [publishers, setPublishers] = useState([]);
  const [botName, setBotName] = useState('');
  const [siteScope, setSiteScope] = useState('');
  const [publisherName, setPublisherName] = useState('');
  const [createdToken, setCreatedToken] = useState('');
  const headers = () => ({ Authorization: `Bearer ${token}` });

  async function load() {
    const [botResponse, publisherResponse] = await Promise.all([
      fetch('/api/admin/bots', { headers: headers() }),
      fetch('/api/admin/publishers', { headers: headers() }),
    ]);
    if (!botResponse.ok || !publisherResponse.ok) {
      setMessage(`Erreur identités ${botResponse.ok ? publisherResponse.status : botResponse.status}`);
      return;
    }
    setBots(await botResponse.json());
    setPublishers(await publisherResponse.json());
  }

  async function create(kind, body) {
    setCreatedToken('');
    const response = await fetch(`/api/admin/${kind}`, {
      method: 'POST',
      headers: { ...headers(), 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const text = await response.text();
    if (!response.ok) {
      setMessage(`Erreur création ${response.status} : ${text}`);
      return;
    }
    const created = JSON.parse(text);
    setCreatedToken(created.token);
    setMessage(`${kind === 'bots' ? 'Bot' : 'Publisher'} créé`);
    await load();
  }

  async function revoke(kind, id) {
    const response = await fetch(`/api/admin/${kind}/${id}`, { method: 'DELETE', headers: headers() });
    if (!response.ok) {
      setMessage(`Erreur révocation ${response.status}`);
      return;
    }
    setMessage('Identité révoquée');
    await load();
  }

  const rows = (items, kind) => items.map((identity) => (
    <tr key={identity.id}>
      <td>{identity.name}</td>
      <td>{identity.site_scope === '*' ? 'Sites non répertoriés' : identity.site_scope}</td>
      <td>{new Date(identity.created_at).toLocaleString()}</td>
      <td>{identity.revoked_at ? new Date(identity.revoked_at).toLocaleString() : 'Actif'}</td>
      <td>{!identity.revoked_at && <button className="danger" onClick={() => revoke(kind, identity.id)}>Révoquer</button>}</td>
    </tr>
  ));

  return <section>
    <div className="section-title"><div><h2>Bots et publishers</h2><p>Les jetons ne sont affichés qu’à leur création.</p></div><button onClick={load}>Actualiser</button></div>
    {createdToken && <div className="token-result"><strong>Jeton créé — copiez-le maintenant</strong><code>{createdToken}</code></div>}
    <div className="identity-forms">
      <form onSubmit={(event) => { event.preventDefault(); create('bots', { name: botName, site_scope: siteScope || botName }); }}>
        <h3>Nouveau bot</h3>
        <label>Nom <input required pattern="[a-z0-9-]+" value={botName} onChange={(event) => setBotName(event.target.value)} /></label>
        <label>Site autorisé <input required pattern="[a-z0-9-]+" value={siteScope} onChange={(event) => setSiteScope(event.target.value)} /></label>
        <button type="submit">Créer le bot</button>
      </form>
      <form onSubmit={(event) => { event.preventDefault(); create('publishers', { name: publisherName }); }}>
        <h3>Nouveau publisher</h3>
        <label>Nom <input required pattern="[a-z0-9-]+" value={publisherName} onChange={(event) => setPublisherName(event.target.value)} /></label>
        <p>Accès à tous les sites non répertoriés.</p>
        <button type="submit">Créer le publisher</button>
      </form>
    </div>
    <h3>Bots</h3>
    <div className="table-wrap"><table><thead><tr><th>Nom</th><th>Site</th><th>Créé</th><th>État</th><th /></tr></thead><tbody>{rows(bots, 'bots')}</tbody></table></div>
    <h3>Publishers</h3>
    <div className="table-wrap"><table><thead><tr><th>Nom</th><th>Portée</th><th>Créé</th><th>État</th><th /></tr></thead><tbody>{rows(publishers, 'publishers')}</tbody></table></div>
  </section>;
}

function App() {
  const [token, setToken] = useState('');
  const [sites, setSites] = useState([]);
  const [name, setName] = useState('');
  const [visibility, setVisibility] = useState('unlisted');
  const [html, setHtml] = useState('<!doctype html><html><body><h1>Nouvelle page</h1></body></html>');
  const [asset, setAsset] = useState(null);
  const [message, setMessage] = useState('');
  const headers = () => ({ Authorization: `Bearer ${token}` });

  async function loadSites() {
    const response = await fetch('/api/sites', { headers: headers() });
    setMessage(response.ok ? '' : `Erreur ${response.status}`);
    if (response.ok) setSites(await response.json());
  }
  async function publish(event) {
    event.preventDefault();
    const response = await fetch('/api/publish', { method: 'POST', headers: { ...headers(), 'Content-Type': 'application/json' }, body: JSON.stringify({ site: name, path: 'index.html', content: html, visibility }) });
    const body = await response.text();
    setMessage(response.ok ? `Page publiée : ${JSON.parse(body).url}` : `Erreur ${response.status} : ${body}`);
    if (response.ok) loadSites();
  }
  async function upload() {
    if (!asset) return;
    const response = await fetch(`/api/assets/${encodeURIComponent(name)}/${encodeURIComponent(asset.name)}`, { method: 'PUT', headers: headers(), body: asset });
    setMessage(response.ok ? `Asset déposé : ${JSON.parse(await response.text()).url}` : `Erreur asset ${response.status}`);
  }

  return <main>
    <header><h1>BotPages</h1><p>Publiez des sites statiques sans redéployer le serveur.</p></header>
    <section><h2>Administration</h2><label>Jeton administrateur <input type="password" value={token} onChange={(event) => setToken(event.target.value)} autoComplete="off" /></label><button onClick={loadSites}>Afficher les sites</button><ul>{sites.map((site) => <li key={site.slug}><strong>{site.name}</strong> · {site.visibility} · <code>{site.slug}</code></li>)}</ul></section>
    <IdentityPanel token={token} setMessage={setMessage} />
    <section><h2>Publier une page</h2><form onSubmit={publish}><label>Nom du site <input required pattern="[a-z0-9-]+" value={name} onChange={(event) => setName(event.target.value)} /></label><label>Visibilité <select value={visibility} onChange={(event) => setVisibility(event.target.value)}><option value="unlisted">Publique non répertoriée</option><option value="protected">Protégée</option></select></label><label>index.html <textarea rows="9" value={html} onChange={(event) => setHtml(event.target.value)} /></label><button type="submit">Publier</button></form><label>Asset (image, vidéo, CSS, JS) <input type="file" onChange={(event) => setAsset(event.target.files?.[0])} /></label><button onClick={upload}>Déposer l’asset</button></section>
    <p role="status">{message}</p>
  </main>;
}

createRoot(document.getElementById('root')).render(<App />);
