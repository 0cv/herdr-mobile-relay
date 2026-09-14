const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.4-382-fb0da72faf14ac43/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
