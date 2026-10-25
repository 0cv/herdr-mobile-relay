const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.0-468-c994d1082fa26971/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
