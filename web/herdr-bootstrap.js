const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.0-421-75c8578a2021dc8a/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
