const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.1-472-4c58f9218ee9b9a2/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
