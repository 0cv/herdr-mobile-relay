const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.10-415-c5435a791a219afa/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
