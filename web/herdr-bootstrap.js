const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.0-490-d1262d57620f2e22/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
