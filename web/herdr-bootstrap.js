const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.11-417-96b19dec204d3d9a/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
