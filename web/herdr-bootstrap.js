const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.4-409-1adf25c652489d98/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
