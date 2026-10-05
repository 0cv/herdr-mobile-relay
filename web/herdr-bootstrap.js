const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.7-398-616b16e20d07be76/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
