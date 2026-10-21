const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.0-415-a6db7feb794382d7/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
