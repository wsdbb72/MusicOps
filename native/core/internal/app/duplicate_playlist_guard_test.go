package app

import "testing"

func TestPlaylistCandidateIDsUsesAlbumMembersOnlyAsFallback(t *testing.T) {
	index := playlistLibraryIndex{
		titleCandidates: map[string][]string{"song": {"title-hit"}},
		albums:          map[[2]string][]string{{"artist", "album"}: {"album-hit"}},
		albumMembers:    map[string][]string{playlistMatchKey(hotSlowCompilationAlbum): {"member-a", "member-b"}},
		tracks:          map[[3]string][]string{},
	}
	row := playlistTrack{Artist: "Artist", Album: "Album", Title: "Song"}
	got := index.playlistCandidateIDs(row)
	if len(got) != 2 || got[0] != "album-hit" || got[1] != "title-hit" {
		t.Fatalf("matching title should not attach every album member: %v", got)
	}
	row.Album = hotSlowCompilationAlbum
	row.Title = "Different Path Title"
	got = index.playlistCandidateIDs(row)
	if len(got) != 2 {
		t.Fatalf("unmatched title should use album members as a fallback: %v", got)
	}
}
