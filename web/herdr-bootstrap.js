const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.26.1-518-d9772af51c4d023f/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
