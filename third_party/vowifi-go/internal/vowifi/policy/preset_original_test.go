package policy

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

func TestOriginalCarrierPresetAssetsRemainExact(t *testing.T) {
	wantHashes := map[string]string{
		"2degrees_nz_53024.yaml":   "e90a9605c10f3c69c79a3fce53ccfa5e0b80b0b4bbb43de6261997f740833a17",
		"ais_th_52001.yaml":        "f02b536317933c00c1960bfcceaf1b3f2ec8e22e1106917854231953832ccaa0",
		"ais_th_52003.yaml":        "7b7c444a9c641d19c9ea184997c27c24eacc52d6b31db729b448fe9e1f50d0fd",
		"att_310280.yaml":          "7337399037af7fbe67874b83f8f0c95ff0a1c588ec49ee41a404cf33e04fd054",
		"att_310410.yaml":          "06c030e8bd636271f9a23400cd4a622187cb38792fdc01b5422a4d719ff59b95",
		"cmhk_45412.yaml":          "b38938fedd7069a0f46959724c133e7c4ebab15369346d7623f901bcb4c603d5",
		"cmhk_45413.yaml":          "c7bc2f772644f110cce24b1e49dedde1ddcf12d2420400e8b7bf39467e08e690",
		"csl_454000.yaml":          "250f9ec40e6c6a4203a5f29063491317c96276753c42980dd37c491f7d3cfc56",
		"cteuk_23433.yaml":         "52ec96bdca5e6e789862f47d9200eb5de3ddacd103c3374882898c8ec8b74517",
		"dito_51566.yaml":          "529cb896a1cf83503c1d24fc5dec3416af668fb7f6bd20fc70fa2748103248ed",
		"docomo_44010.yaml":        "19281f52cf4cbbdd6973be63704b7d9438088a4fefa5d6e96ec9c112286dacdb",
		"ee_uk_23430.yaml":         "cae2b7373401f83ad468bdc4bee3129520d33cbaadcdda5b461d9011d427e0dc",
		"ee_uk_23431.yaml":         "6ce981d9065cfcba1beb69385ffda4e3305aec6878fcc0ed3f3b9847f0176b5a",
		"ee_uk_23432.yaml":         "0ccae7020f4151d0e31f3440a1a09bb87f39ee16362a0c556e1ac02824f8a559",
		"elisa_ee_24802.yaml":      "e7d833e821deda5145033d70536b4820ac1d4685ef4a272996df7e66fc355243",
		"giffgaff_23410.yaml":      "06f01db9a8c6f0d57569174aa596b9a67e2fa568d06449b50c10b2a41611d3ef",
		"globe_ph_51502.yaml":      "66985e55c849dd758090d3413c5295fa7da4029b8b0af38afb3d06debe34f190",
		"hotlink_my_50212.yaml":    "e3aae045a505983376a7b9627a5e43e13af136a747f545087c7b97d0a8a67c8c",
		"kddi_44051.yaml":          "b6ea22aecd795b8265b7dddd59e8a83831f11201515b8dc9439b1892d5f0fd45",
		"kpn_nl_20408.yaml":        "86d9fb6ba92179511f4aa7c13b1a10f2658ca4485df82b337b87dc2348c94eb9",
		"lebara_uk_23487.yaml":     "4c469de01cd7fc78119045a5855547d1b7ed862e557d0c8486770eecc038605a",
		"lycamobile_uk_23426.yaml": "4a176cd79fc7390b8e49a35cb3c3a6c9153a6a0a086e4d22b7f69bad73b92687",
		"mtn_ng_62130.yaml":        "64d0de150cdd6f393ed78faecf92b0d2b70ca1625eb0526b0dc8077e436cc6cc",
		"o2_de_26203.yaml":         "4772a3f0babe9f5da7cc160315f3344b63e0221b1d94fb8d18ee8739eec0d5a6",
		"o2_de_26207_alias.yaml":   "a9a182ff9bc59e262d0c348dc8a65ff9c86cdd8ded3fe04803f1c1b7ea214cf2",
		"one_nz_53001.yaml":        "758732596bb61bdd8cdec8658e97e4842eed9d4d879e775a4c236bdc8840ae3e",
		"oneglobal_23425.yaml":     "46a5450e9403217115f7a82d9174a18feb544b9828641b83f93c761114abb5d5",
		"orange_fr_20801.yaml":     "68d6d1cc830316d17cd607d65cf176e0f92b27393bd899735136a0fabc507328",
		"smart_ph_51503.yaml":      "fca9263e07be5707fa1365879179462757e554261512dbcc58cfd0fac390086e",
		"softbank_44020.yaml":      "6398fe7ccbae656d6a85589ba1cf903b4d07e4c22978d7fff92aad11ccfb1dd7",
		"spark_nz_53005.yaml":      "dbe82d043ed10c283ad2c16ca96db2f9538514d07e1a21213d285a5eec3cdf9b",
		"sunrise_22802.yaml":       "2160911e6d4fca664fbc9c474eee71a468d282703760f3c87e99df9043872e7b",
		"telekom_de_26201.yaml":    "aac0d40b575b4ab5625ac437b3ea7cd228633f87b1e2b5f3ddd12f7024bd4455",
		"three_hk_454003.yaml":     "a3a84a436646cddd80df30aa2493f4b4076f08664a478d0b954c4142ef196d61",
		"three_uk_234020.yaml":     "49cf6d64d8e180ff475e28718f70f6bdf6da1b388fdd8996cc6921d8e3d6d9c3",
		"tmobile_310240.yaml":      "bed3bfcb871282eab4c446004c1837c39426aa94e1206ab18c41f52aa82c4cb1",
		"tmobile_310260.yaml":      "57619113de3cfbe9ddb4afda13885501415f534f580fc55a8e7f54a15f2b93a6",
		"vodafone_de_26202.yaml":   "6881882fa8e11ecbbaa3f4d44b1bc8d7f16deba34a08f4d7d53268e96b7ab2a6",
		"vodafone_nl_20404.yaml":   "ca847e87abe516603cd92f05ef0a9e5ba8ad0869f7cca3ea2aabe9f49694df8b",
		"vodafone_uk_23415.yaml":   "4a83ec1b4d83d853d627b008cdc40ecfc40fd521dd183e303c74b5e79df12808",
		"ymobile_44000.yaml":       "6804965637e8d5d279b9a2eeb6a2dcd5ecd01bd8b4745ee7a5486b17d1c30c72",
	}
	entries, err := carrierPresetFiles.ReadDir("presets")
	if err != nil {
		t.Fatalf("read embedded preset directory: %v", err)
	}
	if len(entries) != len(wantHashes) {
		t.Fatalf("embedded preset files = %d, want %d", len(entries), len(wantHashes))
	}
	for name, want := range wantHashes {
		data, err := carrierPresetFiles.ReadFile("presets/" + name)
		if err != nil {
			t.Fatalf("read original preset %s: %v", name, err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != want {
			t.Errorf("original preset %s hash = %s, want %s", name, got, want)
		}
	}
}

func TestEmbeddedCarrierPresetInventory(t *testing.T) {
	want := []string{
		"204004", "204008", "208001", "228002", "234010", "234015", "234020", "234025", "234026", "234030", "234031",
		"234032", "234033", "234087", "248002", "262001", "262002", "262003", "262007", "310240",
		"310260", "310280", "310410", "440000", "440010", "440020", "440051", "454000", "454003", "454012", "454013",
		"502012", "515002", "515003", "515066", "520001", "520003", "530001", "530005", "530024", "621030",
	}
	got := make([]string, 0, len(embeddedCarrierPresets))
	for key := range embeddedCarrierPresets {
		got = append(got, key)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("embedded carrier preset inventory = %v, want %v", got, want)
	}
}
