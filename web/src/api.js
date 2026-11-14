async function request (method, url, body) {
  const opt = { method, headers: { 'Content-Type': 'application/json' } }
  if (body !== undefined) opt.body = JSON.stringify(body)
  const res = await fetch(url, opt)
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(data.error || `${method} ${url} -> ${res.status}`)
  return data
}

export const api = {
  health: () => request('GET', '/api/health'),
  hostInfo: () => request('GET', '/api/host-info'),
  project: projectDir =>
    request('GET', `/api/project?projectDir=${encodeURIComponent(projectDir || '')}`),
  bundleManifest: path =>
    request('GET', `/api/bundle-manifest?path=${encodeURIComponent(path)}`),
  pack: body => request('POST', '/api/pack', body),
  install: body => request('POST', '/api/install', body),
  job: (id, offset) => request('GET', `/api/jobs/${id}?offset=${offset}`),
  fs: path =>
    request('GET', `/api/fs${path ? `?path=${encodeURIComponent(path)}` : ''}`),
  defaultInstallDir: (name, mode, user) =>
    request('GET', `/api/default-install-dir?name=${encodeURIComponent(name || '')}&mode=${encodeURIComponent(mode || 'remote')}&user=${encodeURIComponent(user || '')}`)
}
