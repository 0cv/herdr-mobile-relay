const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.0-370-29f82d25fbe15652/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
