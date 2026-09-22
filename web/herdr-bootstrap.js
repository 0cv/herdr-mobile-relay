const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.3-388-fe577b977a1376c7/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
