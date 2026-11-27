const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.27.0-562-9dc0607b43b47c58/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
