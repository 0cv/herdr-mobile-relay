const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.0-466-c7d9b422e258378d/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
