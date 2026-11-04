package src

import (
	"encoding/json"
	"strings"
	"unicode"
)

// legacyGetProgramData is getProgramData as it was before the programmes
// were indexed by channel (copied verbatim, except that getCategory no
// longer takes the filters it never used).
func legacyGetProgramData(xepgChannel XEPGChannelStruct) (xepgXML XMLTV, err error) {
	var xmltvFile = System.Folder.Data + xepgChannel.XmltvFile
	var channelID = xepgChannel.XMapping

	var xmltv XMLTV

	if strings.Contains(xmltvFile, "Threadfin Dummy") {
		xmltv = createDummyProgram(xepgChannel)
	} else {
		if xepgChannel.XmltvFile != "" {
			err = getLocalXMLTV(xmltvFile, &xmltv)
			if err != nil {
				return
			}
		}
	}

	for _, xmltvProgram := range xmltv.Program {
		if xmltvProgram.Channel == channelID {
			var program = &Program{}

			// Channel ID
			program.Channel = xepgChannel.XChannelID
			program.Start = xmltvProgram.Start
			program.Stop = xmltvProgram.Stop

			// Title
			if len(xmltvProgram.Title) > 0 {
				if !Settings.EnableNonAscii {
					xmltvProgram.Title[0].Value = strings.TrimSpace(strings.Map(func(r rune) rune {
						if r > unicode.MaxASCII {
							return -1
						}
						return r
					}, xmltvProgram.Title[0].Value))
				}
				program.Title = xmltvProgram.Title
			}

			filters := []FilterStruct{}
			for _, filter := range Settings.Filter {
				filter_json, _ := json.Marshal(filter)
				f := FilterStruct{}
				json.Unmarshal(filter_json, &f)
				filters = append(filters, f)
			}
			_ = filters // getCategory no longer takes the (unused) filters

			// Category (Kategorie)
			getCategory(program, xmltvProgram, xepgChannel)

			// Sub-Title
			program.SubTitle = xmltvProgram.SubTitle

			// Description
			program.Desc = xmltvProgram.Desc

			// Credits : (Credits)
			program.Credits = xmltvProgram.Credits

			// Rating (Bewertung)
			program.Rating = xmltvProgram.Rating

			// StarRating (Bewertung / Kritiken)
			program.StarRating = xmltvProgram.StarRating

			// Country (Länder)
			program.Country = xmltvProgram.Country

			// Program icon (Poster / Cover)
			getPoster(program, xmltvProgram, xepgChannel, Settings.ForceHttps)

			// Language (Sprache)
			program.Language = xmltvProgram.Language

			// Episodes numbers (Episodennummern)
			getEpisodeNum(program, xmltvProgram, xepgChannel)

			// Video (Videoparameter)
			getVideo(program, xmltvProgram, xepgChannel)

			// Date (Datum)
			program.Date = xmltvProgram.Date

			// Previously shown (Wiederholung)
			program.PreviouslyShown = xmltvProgram.PreviouslyShown

			// New (Neu)
			program.New = xmltvProgram.New

			// Live
			program.Live = xmltvProgram.Live

			// Premiere
			program.Premiere = xmltvProgram.Premiere

			xepgXML.Program = append(xepgXML.Program, program)

		}

	}

	return
}
