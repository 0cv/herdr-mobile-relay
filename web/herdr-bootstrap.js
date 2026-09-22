const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.3-386-701ad212682c8ed6/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
