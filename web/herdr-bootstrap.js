const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.10-410-7b6e202f8dc19c98/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
