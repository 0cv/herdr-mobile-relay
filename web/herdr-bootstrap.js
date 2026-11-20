const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.26.1-512-5c11d4af8e57f186/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
