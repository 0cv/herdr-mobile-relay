const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.27.0-563-daa512cbac275c96/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
