const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.27.0-560-334682063ebccbfa/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
