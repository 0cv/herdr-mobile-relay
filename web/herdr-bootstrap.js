const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.2-473-706187ad85e52b05/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
