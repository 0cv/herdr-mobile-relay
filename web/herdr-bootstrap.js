const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.1-495-9482bdba76162db7/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
