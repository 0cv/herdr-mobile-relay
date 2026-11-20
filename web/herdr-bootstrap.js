const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.26.1-514-55ee4bde0beeb07e/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
