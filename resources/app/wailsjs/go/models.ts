export namespace main {
	
	export class SwitchTitle {
	    name: string;
	    titleId: string;
	    icon: string;
	    region: string;
	    release_date: string;
	
	    static createFrom(source: any = {}) {
	        return new SwitchTitle(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.titleId = source["titleId"];
	        this.icon = source["icon"];
	        this.region = source["region"];
	        this.release_date = source["release_date"];
	    }
	}

}

