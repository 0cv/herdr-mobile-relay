const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.27.0-552-6cb1bf00f6ae1d97/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
