package elasticsearch_indexing

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestGetPostsBatchForIndexingSkipsOrphanedPosts(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`CREATE TABLE Channels (id TEXT, teamid TEXT, type TEXT)`,
		`CREATE TABLE Posts (id TEXT, createat INTEGER, updateat INTEGER, editat INTEGER,
			deleteat INTEGER, ispinned BOOLEAN, userid TEXT, channelid TEXT, rootid TEXT,
			originalid TEXT, message TEXT, type TEXT, props TEXT, hashtags TEXT,
			filenames TEXT, fileids TEXT, hasreactions BOOLEAN, remoteid TEXT)`,
		`INSERT INTO Channels VALUES ('channel', 'team', 'O')`,
		`INSERT INTO Posts VALUES ('orphan', 1, 1, 0, 0, 0, 'user', 'missing', '', '',
			'orphan message', '', '{}', '', '[]', '[]', 0, NULL)`,
		`INSERT INTO Posts VALUES ('valid', 2, 2, 0, 0, 0, 'user', 'channel', '', '',
			'valid message', '', '{}', '', '[]', '[]', 0, NULL)`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}

	posts, err := (&indexingPostStore{db: db}).GetPostsBatchForIndexing(0, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 || posts[0].Id != "valid" || posts[0].TeamId != "team" || posts[0].ChannelType != "O" {
		t.Fatalf("unexpected indexed posts: %#v", posts)
	}
}
