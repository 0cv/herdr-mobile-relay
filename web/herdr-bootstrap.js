const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.0-410-ae243604b5e460aa/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
