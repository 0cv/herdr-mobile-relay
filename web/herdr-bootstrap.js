const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.27.0-522-2540c4d96fcb9e43/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
