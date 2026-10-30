const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.5-484-e2cd66df5f1cff84/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
