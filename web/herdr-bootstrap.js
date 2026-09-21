const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.3-384-3b518afd98bb6838/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
