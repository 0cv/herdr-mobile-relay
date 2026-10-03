const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.4-404-aff286a6e0d642dd/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
