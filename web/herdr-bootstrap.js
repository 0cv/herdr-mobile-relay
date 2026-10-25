const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.1-470-88e15278f0fd9046/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
