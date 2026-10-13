const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.7-405-fee55e244a914d2a/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
