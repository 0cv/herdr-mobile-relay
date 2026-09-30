const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.3-383-e0c0f18fff52fca0/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
