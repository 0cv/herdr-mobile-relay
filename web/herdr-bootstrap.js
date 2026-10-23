const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.12-418-bbcae882b7079ef5/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
