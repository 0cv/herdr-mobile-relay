const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.0-459-d62d66d0202a6e67/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
