const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.24.0-488-a5d176ec889de066/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
