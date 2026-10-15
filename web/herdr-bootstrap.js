const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.10-409-721ce9d2da89110d/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
