const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.0-461-650075d64b5452c4/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
