const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.6-397-9cdabaf72e009620/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
