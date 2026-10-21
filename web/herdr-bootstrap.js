const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.11-416-a8c15b39395202e3/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
