const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.1-391-1821ecbdd70f5abb/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
