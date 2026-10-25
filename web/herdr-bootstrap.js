const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.0-463-e5c82de198403e7f/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
