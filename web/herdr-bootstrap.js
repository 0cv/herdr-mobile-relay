const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.6-409-0d2cdde9272c7b47/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
