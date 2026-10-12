const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.7-403-3bbc8549f8dd7a96/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
