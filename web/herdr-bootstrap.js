const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.2-392-cb24b35231ceb9cb/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
